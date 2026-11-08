//go:build linux || windows

package hostmon

import (
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

// readGPU asks nvidia-smi (about 35 ms on the GB10). A host without it, or
// one where it hangs or fails, reports no GPU figures. A driver that does
// not know one of the fields fails the whole query, so it is asked the
// first four alone from then on.
func readGPU() gpuReading {
	smiOnce.Do(func() { smiPath, _ = exec.LookPath("nvidia-smi") })
	if smiPath == "" {
		return gpuReading{}
	}
	fields := smiFields
	if smiShort.Load() {
		fields = "name,utilization.gpu,temperature.gpu,power.draw"
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
