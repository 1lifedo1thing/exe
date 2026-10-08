//go:build linux

package hostmon

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"exe/internal/hostinfo"
)

// Supported: Linux reads all of it from /proc, /sys and nvidia-smi.
const Supported = true

func readHost() counters {
	c := counters{at: time.Now()}
	if f, err := os.Open("/proc/stat"); err == nil {
		c.cpuBusy, c.cpuTotal, c.cpuOK = parseStat(f)
		f.Close()
	}
	mem := hostinfo.Mem()
	c.memTotal, c.memAvail = mem.Total, mem.Available
	if f, err := os.Open("/proc/net/dev"); err == nil {
		c.net = parseNetDev(f, physical("/sys/class/net/"))
		f.Close()
	}
	if f, err := os.Open("/proc/diskstats"); err == nil {
		c.disk = parseDiskstats(f, physical("/sys/block/"))
		f.Close()
	}
	c.gpu = readGPU()
	host, _ := os.Hostname()
	c.info = Info{Host: host, Cores: runtime.NumCPU(), MemoryTotal: c.memTotal, GPU: c.gpu.name, Net: names(c.net), Disks: names(c.disk)}
	return c
}

// physical keeps the devices backed by hardware — a NIC or a disk has a
// `device` link under /sys; loopback, bridges, VM taps, Docker veths,
// tailscale0, loop and device-mapper devices have none. Counting only these
// counts each byte once: a VM's traffic crosses its tap, the bridge and the
// NIC, a filesystem's its dm device and its disk.
func physical(dir string) func(string) bool {
	return func(name string) bool {
		_, err := os.Stat(dir + name + "/device")
		return err == nil
	}
}

// parseStat reads the aggregate "cpu" line: busy is every jiffy but idle
// and iowait; guest time is already inside user and nice.
func parseStat(r io.Reader) (busy, total uint64, ok bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var v [8]uint64
		for i := 0; i < 8 && i+1 < len(f); i++ {
			n, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return 0, 0, false
			}
			v[i] = n
		}
		for _, n := range v {
			total += n
		}
		return total - v[3] - v[4], total, true
	}
	return 0, 0, false
}

// parseNetDev reads /proc/net/dev: received bytes are the first figure
// after the name, transmitted the ninth.
func parseNetDev(r io.Reader, keep func(string) bool) map[string][2]uint64 {
	out := map[string][2]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		name = strings.TrimSpace(name)
		if !ok || !keep(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 == nil && err2 == nil {
			out[name] = [2]uint64{rx, tx}
		}
	}
	return out
}

// parseDiskstats reads /proc/diskstats: sectors read are the sixth field,
// sectors written the tenth, and a sector there is always 512 bytes.
func parseDiskstats(r io.Reader, keep func(string) bool) map[string][2]uint64 {
	out := map[string][2]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || !keep(f[2]) {
			continue
		}
		rd, err1 := strconv.ParseUint(f[5], 10, 64)
		wr, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 == nil && err2 == nil {
			out[f[2]] = [2]uint64{rd * 512, wr * 512}
		}
	}
	return out
}

var smiOnce sync.Once
var smiPath string

// readGPU asks nvidia-smi (about 25 ms on the GB10). A host without it, or
// one where it hangs or fails, reports no GPU figures.
func readGPU() gpuReading {
	smiOnce.Do(func() { smiPath, _ = exec.LookPath("nvidia-smi") })
	if smiPath == "" {
		return gpuReading{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, smiPath, "--query-gpu=name,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return gpuReading{}
	}
	return parseSMI(string(out))
}

// parseSMI reads one "name, util, temp, power" line per GPU: utilisation
// averaged, the hottest temperature, power summed. "[N/A]" is no figure —
// the GB10 has no memory of its own to report, and some boards no power.
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
		if g.name == "" {
			g.name = f[0]
		}
		if v, ok := smiNum(f[1]); ok {
			util += v
			nUtil++
		}
		if v, ok := smiNum(f[2]); ok && (g.temp == nil || v > *g.temp) {
			g.temp = num(v)
		}
		if v, ok := smiNum(f[3]); ok {
			power += v
			nPower++
		}
	}
	if nUtil > 0 {
		g.util = num(util / float64(nUtil))
	}
	if nPower > 0 {
		g.power = num(power)
	}
	return g
}

func smiNum(s string) (float64, bool) {
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func names(m map[string][2]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
