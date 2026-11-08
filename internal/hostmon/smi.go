//go:build linux || windows

package hostmon

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The NVIDIA cards, through nvidia-smi: the same program, and the same
// answer, on Linux and on Windows.

var smiOnce sync.Once
var smiPath string
var smiShort atomic.Bool // the driver refused the full query once: ask the four figures alone

// smiFields is what nvidia-smi is asked, in parseSMI's order: the four the
// Control Strip shows, then the Activity Monitor's.
const smiFields = "name,utilization.gpu,temperature.gpu,power.draw," +
	"utilization.memory,memory.used,memory.total,power.limit,fan.speed,clocks.sm,clocks.max.sm,pstate,utilization.encoder,utilization.decoder"

const smiShortFields = "name,utilization.gpu,temperature.gpu,power.draw"

// readGPU gives the cards' newest figures. nvidia-smi is started once and
// kept going, printing a line a card every two seconds (-lms 2000): started
// for each reading it cost about 39 ms of CPU on the GB10, mostly its
// library starting, and kept going about 7 ms a line. Until its first
// lines come, and while it is not running, a reading asks it once, as
// before. A host without it reports no GPU figures.
func readGPU() gpuReading {
	smiOnce.Do(func() { smiPath, _ = exec.LookPath("nvidia-smi") })
	if smiPath == "" {
		return gpuReading{}
	}
	if g, ok := smiLoop.read(); ok {
		return g
	}
	return readGPUOnce()
}

// readGPUOnce asks nvidia-smi once. A driver that does not know one of the
// fields fails the whole query, so it is asked the first four alone from
// then on.
func readGPUOnce() gpuReading {
	fields := smiFields
	if smiShort.Load() {
		fields = smiShortFields
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, smiPath, "--query-gpu="+fields, "--format=csv,noheader,nounits")
	quiet(cmd)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && !smiShort.Load() {
			smiShort.Store(true)
		}
		return gpuReading{}
	}
	return parseSMI(string(out))
}

// smiLoop is the nvidia-smi kept going: each card's newest line, by its
// index. A line older than six seconds is a card that stopped answering.
var smiLoop loopReader

type loopReader struct {
	mu      sync.Mutex
	lines   map[int]smiLine
	running bool
	next    time.Time // when it may be started again, after it ended
}

type smiLine struct {
	text string
	at   time.Time
}

func (l *loopReader) read() (gpuReading, bool) {
	l.mu.Lock()
	if !l.running && !time.Now().Before(l.next) {
		l.start()
	}
	var idx []int
	for i, ln := range l.lines {
		if time.Since(ln.at) < 6*time.Second {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	var b strings.Builder
	for _, i := range idx {
		b.WriteString(l.lines[i].text)
		b.WriteByte('\n')
	}
	l.mu.Unlock()
	if len(idx) == 0 {
		return gpuReading{}, false
	}
	return parseSMI(b.String()), true
}

// start runs nvidia-smi in its loop, with l.mu held. It ends with the
// daemon: the pipe it writes to closes, and its next line kills it. One
// that ends with no line at all refused a field, and is started again
// with the four alone; one that ran a while is started again in half a
// minute.
func (l *loopReader) start() {
	fields := smiFields
	if smiShort.Load() {
		fields = smiShortFields
	}
	cmd := exec.Command(smiPath, "--query-gpu=index,"+fields, "--format=csv,noheader,nounits", "-lms", "2000")
	quiet(cmd)
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		l.next = time.Now().Add(time.Minute)
		return
	}
	l.running = true
	go func() {
		got := false
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			i, rest, ok := strings.Cut(sc.Text(), ",")
			n, err := strconv.Atoi(strings.TrimSpace(i))
			if !ok || err != nil {
				continue
			}
			l.mu.Lock()
			if l.lines == nil {
				l.lines = map[int]smiLine{}
			}
			l.lines[n] = smiLine{text: strings.TrimSpace(rest), at: time.Now()}
			l.mu.Unlock()
			got = true
		}
		cmd.Wait()
		l.mu.Lock()
		l.running = false
		if !got && !smiShort.Load() {
			smiShort.Store(true)
		} else {
			l.next = time.Now().Add(30 * time.Second)
		}
		l.mu.Unlock()
	}()
}

// parseSMI reads one "name, util, temp, power" line per GPU: utilisation
// averaged, the hottest temperature, power summed, and with more than one
// card each card's own. "[N/A]" is no figure — the GB10 has no memory of
// its own to report, and some boards no power.
func parseSMI(s string) gpuReading {
	var g gpuReading
	var util, power float64
	var nUtil, nPower int
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		var card GPU
		if v, ok := smiNum(f[1]); ok {
			card.Util = num(v)
			util += v
			nUtil++
		}
		if v, ok := smiNum(f[2]); ok {
			card.Temp = num(v)
			if g.temp == nil || v > *g.temp {
				g.temp = num(v)
			}
		}
		if v, ok := smiNum(f[3]); ok {
			card.Power = num(v)
			power += v
			nPower++
		}
		g.names = append(g.names, f[0])
		g.cards = append(g.cards, card)
		d := Card{Util: card.Util, Temp: card.Temp, Power: card.Power}
		ci := CardInfo{Name: f[0]}
		if len(f) >= 14 {
			d.MemUtil, d.Fan, d.Clock, d.Enc, d.Dec = smiPtr(f[4]), smiPtr(f[8]), smiPtr(f[9]), smiPtr(f[12]), smiPtr(f[13])
			if v, ok := smiNum(f[5]); ok {
				d.MemUsed = num(v * (1 << 20)) // MiB
			}
			if v, ok := smiNum(f[6]); ok && v > 0 {
				ci.MemTotal = num(v * (1 << 20))
			}
			if !strings.HasPrefix(f[11], "[") {
				d.PState = f[11]
			}
			ci.PowerLimit, ci.ClockMax = smiPtr(f[7]), smiPtr(f[10])
			ci.Fan, ci.Video = d.Fan != nil, d.Enc != nil || d.Dec != nil
		}
		g.detail = append(g.detail, d)
		g.detailInfo = append(g.detailInfo, ci)
	}
	if nUtil > 0 {
		g.util = num(util / float64(nUtil))
	}
	if nPower > 0 {
		g.power = num(power)
	}
	switch {
	case len(g.names) == 1:
		g.name, g.names, g.cards = g.names[0], nil, nil
	case len(g.names) > 1 && slices.Equal(g.names, slices.Repeat(g.names[:1], len(g.names))):
		g.name = strconv.Itoa(len(g.names)) + " × " + g.names[0]
	case len(g.names) > 1:
		g.name = strings.Join(g.names, " + ")
	}
	return g
}

func smiNum(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func smiPtr(s string) *float64 {
	if v, ok := smiNum(s); ok {
		return num(v)
	}
	return nil
}
