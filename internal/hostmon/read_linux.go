//go:build linux

package hostmon

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"exe/internal/hostinfo"
)

// Supported: Linux reads all of it from /proc, /sys and nvidia-smi.
const Supported = true

func readHost() counters {
	c := counters{at: time.Now()}
	if f, err := os.Open("/proc/stat"); err == nil {
		c.cpuBusy, c.cpuTotal, c.cpuOK, c.perCPU = parseStat(f)
		f.Close()
	}
	topoOnce.Do(func() { topo = readTopology("/sys/devices/system/cpu", "/proc/cpuinfo") })
	c.freq = readFreqs("/sys/devices/system/cpu", len(topo.cpus))
	c.cpuTemp = readCPUTemp()
	mem := hostinfo.Mem()
	c.memTotal, c.memAvail = mem.Total, mem.Available
	if f, err := os.Open("/proc/net/dev"); err == nil {
		c.net = parseNetDev(f, physical("/sys/class/net/"))
		f.Close()
	}
	var signals map[string]float64
	if b, err := os.ReadFile("/proc/net/wireless"); err == nil {
		signals = parseWireless(string(b))
	}
	ifaces, links := readLinks("/sys/class/net", names(c.net), signals)
	c.links = links
	if f, err := os.Open("/proc/diskstats"); err == nil {
		c.disk = parseDiskstats(f, physical("/sys/block/"))
		f.Close()
	}
	c.gpu = readGPU()
	host, _ := os.Hostname()
	c.info = Info{Host: host, Cores: runtime.NumCPU(), MemoryTotal: c.memTotal, GPU: c.gpu.name, GPUs: c.gpu.names, Net: names(c.net), Disks: names(c.disk),
		CPUs: topo.cpus, CPUKinds: topo.kinds, Cards: c.gpu.detailInfo, Ifaces: ifaces}
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

// parseStat reads the aggregate "cpu" line and each "cpuN" line after it:
// busy is every jiffy but idle and iowait; guest time is already inside
// user and nice. A CPU taken offline has no line.
func parseStat(r io.Reader) (busy, total uint64, ok bool, per map[int][2]uint64) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		b, t, good := statJiffies(f[1:])
		if f[0] == "cpu" {
			if !good {
				return 0, 0, false, nil
			}
			busy, total, ok = b, t, true
			continue
		}
		n, err := strconv.Atoi(f[0][3:])
		if err != nil || n < 0 || !good {
			continue
		}
		if per == nil {
			per = map[int][2]uint64{}
		}
		per[n] = [2]uint64{b, t}
	}
	return busy, total, ok, per
}

func statJiffies(f []string) (busy, total uint64, ok bool) {
	var v [8]uint64
	for i := 0; i < 8 && i < len(f); i++ {
		n, err := strconv.ParseUint(f[i], 10, 64)
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

// topology is the machine's logical CPUs, read once: which kind of core
// each is and which physical core it is a thread of.
type topology struct {
	cpus  []CPU
	kinds []string
}

var topoOnce sync.Once
var topo topology

// readTopology numbers the logical CPUs under a sysfs cpu directory, groups
// threads by (package, core_id) and names their kinds from cpuinfo: x86's
// "model name", or for Arm cores the part number (Cortex-X925 and the like).
func readTopology(sysCPU, cpuinfo string) topology {
	dirs, _ := filepath.Glob(sysCPU + "/cpu[0-9]*")
	ids := []int{}
	for _, d := range dirs {
		if n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(d), "cpu")); err == nil {
			ids = append(ids, n)
		}
	}
	if len(ids) == 0 {
		return topology{}
	}
	slices.Sort(ids)
	n := ids[len(ids)-1] + 1
	kindOf := cpuKinds(cpuinfo)
	var t topology
	kindIdx := map[string]int{}
	coreIdx := map[[2]string]int{}
	t.cpus = make([]CPU, n)
	for i := range n {
		dir := sysCPU + "/cpu" + strconv.Itoa(i)
		kind := kindOf[i]
		if kind == "" {
			kind = "CPU"
		}
		k, ok := kindIdx[kind]
		if !ok {
			k = len(t.kinds)
			kindIdx[kind] = k
			t.kinds = append(t.kinds, kind)
		}
		key := [2]string{readTrim(dir + "/topology/physical_package_id"), readTrim(dir + "/topology/core_id")}
		if key[1] == "" {
			key[1] = "cpu" + strconv.Itoa(i) // no topology: each CPU a core of its own
		}
		c, ok := coreIdx[key]
		if !ok {
			c = len(coreIdx)
			coreIdx[key] = c
		}
		khz, _ := strconv.Atoi(readTrim(dir + "/cpufreq/cpuinfo_max_freq"))
		t.cpus[i] = CPU{Kind: k, Core: c, MaxMHz: khz / 1000}
	}
	return t
}

// cpuKinds reads each processor's kind from /proc/cpuinfo.
func cpuKinds(path string) map[int]string {
	out := map[int]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	cur, impl := -1, ""
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			cur, _ = strconv.Atoi(v)
			impl = ""
		case "model name":
			if cur >= 0 {
				out[cur] = cleanModel(v)
			}
		case "CPU implementer":
			impl = v
		case "CPU part":
			if cur >= 0 && out[cur] == "" {
				if name, ok := armParts[impl+"/"+v]; ok {
					out[cur] = name
				} else {
					out[cur] = "Arm core " + v
				}
			}
		}
	}
	return out
}

// cleanModel drops the marks and the rated clock from an x86 model name:
// "Intel(R) Xeon(R) w9-3475X" → "Intel Xeon w9-3475X".
func cleanModel(s string) string {
	for _, m := range []string{"(R)", "(r)", "(TM)", "(tm)", " CPU", " Processor"} {
		s = strings.ReplaceAll(s, m, "")
	}
	if i := strings.Index(s, " @ "); i > 0 {
		s = s[:i]
	}
	return strings.Join(strings.Fields(s), " ")
}

// armParts names Arm's own cores by implementer 0x41 and part number.
var armParts = map[string]string{
	"0x41/0xd03": "Cortex-A53", "0x41/0xd05": "Cortex-A55", "0x41/0xd07": "Cortex-A57", "0x41/0xd08": "Cortex-A72",
	"0x41/0xd09": "Cortex-A73", "0x41/0xd0a": "Cortex-A75", "0x41/0xd0b": "Cortex-A76", "0x41/0xd0c": "Neoverse-N1",
	"0x41/0xd0d": "Cortex-A77", "0x41/0xd40": "Neoverse-V1", "0x41/0xd41": "Cortex-A78", "0x41/0xd44": "Cortex-X1",
	"0x41/0xd46": "Cortex-A510", "0x41/0xd47": "Cortex-A710", "0x41/0xd48": "Cortex-X2", "0x41/0xd49": "Neoverse-N2",
	"0x41/0xd4d": "Cortex-A715", "0x41/0xd4e": "Cortex-X3", "0x41/0xd4f": "Neoverse-V2", "0x41/0xd80": "Cortex-A520",
	"0x41/0xd81": "Cortex-A720", "0x41/0xd82": "Cortex-X4", "0x41/0xd84": "Neoverse-V3", "0x41/0xd85": "Cortex-X925",
	"0x41/0xd87": "Cortex-A725", "0x41/0xd8e": "Neoverse-N3",
}

// readFreqs is each CPU's present clock in MHz, 0 where it has none.
func readFreqs(sysCPU string, n int) []int {
	if n == 0 {
		return nil
	}
	out := make([]int, n)
	for i := range out {
		khz, _ := strconv.Atoi(readTrim(sysCPU + "/cpu" + strconv.Itoa(i) + "/cpufreq/scaling_cur_freq"))
		out[i] = khz / 1000
	}
	return out
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

// readLinks reads each interface's link under a sysfs class/net root: a
// wireless one has a wireless or phy80211 entry; the link is up when the
// kernel says so (operstate "up", or "unknown" with a carrier, as some
// drivers leave it); a wired link's speed is in Mb/s, and the kernel
// refuses to give it while the link is down.
func readLinks(root string, names []string, signals map[string]float64) ([]Iface, map[string]link) {
	ifaces := make([]Iface, 0, len(names))
	links := map[string]link{}
	for _, name := range names {
		dir := root + "/" + name
		ifc := Iface{Name: name}
		if _, err := os.Stat(dir + "/wireless"); err == nil {
			ifc.Wireless = true
		} else if _, err := os.Stat(dir + "/phy80211"); err == nil {
			ifc.Wireless = true
		}
		state := readTrim(dir + "/operstate")
		l := link{up: state == "up" || (state == "unknown" && readTrim(dir+"/carrier") == "1")}
		if l.up && !ifc.Wireless {
			if mbps, err := strconv.Atoi(readTrim(dir + "/speed")); err == nil && mbps > 0 {
				ifc.Speed = mbps
			}
		}
		if v, ok := signals[name]; ok && l.up {
			l.signal = num(v)
		}
		ifaces = append(ifaces, ifc)
		links[name] = l
	}
	return ifaces, links
}

// parseWireless reads /proc/net/wireless: after two lines of heading, a
// line an associated interface, "wlan0: 0000   55.  -55.  -256 …", whose
// fourth field is the signal level. Only a level in dBm (below zero) is
// kept; some drivers give a relative one instead.
func parseWireless(s string) map[string]float64 {
	out := map[string]float64{}
	for i, line := range strings.Split(s, "\n") {
		if i < 2 {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 3 {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64); err == nil && v < 0 {
			out[strings.TrimSpace(name)] = v
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

var cpuTempOnce sync.Once
var cpuTempFiles []string

// readCPUTemp is the hottest of the CPU's sensors in °C, or nil when the
// machine names none. The sensors are found once (cpuTempSensors).
func readCPUTemp() *float64 {
	cpuTempOnce.Do(func() { cpuTempFiles = cpuTempSensors("/sys") })
	var hottest *float64
	for _, f := range cpuTempFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil || milli <= 0 {
			continue
		}
		if c := milli / 1000; hottest == nil || c > *hottest {
			hottest = num(c)
		}
	}
	return hottest
}

// cpuTempSensors finds the files holding CPU temperatures (millidegrees)
// under a sysfs root: an hwmon chip the kernel names for a CPU (Intel's
// coretemp, AMD's k10temp and zenpower, the ARM boards' cpu_thermal) —
// AMD's Tctl/Tdie only, its other inputs being the dies'; else thermal
// zones typed for a CPU (x86_pkg_temp, cpu-thermal and the like); else
// ACPI zones the firmware names for a CPU cluster — the GB10's TS0E/TS0P
// and TS1E/TS1P (each cluster's efficiency and performance cores), not
// its SoC, GPU or uncore zones.
func cpuTempSensors(root string) []string {
	var out []string
	hw, _ := filepath.Glob(root + "/class/hwmon/hwmon*")
	for _, h := range hw {
		name := readTrim(h + "/name")
		switch name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal":
		default:
			continue
		}
		inputs, _ := filepath.Glob(h + "/temp*_input")
		for _, in := range inputs {
			label := readTrim(strings.TrimSuffix(in, "_input") + "_label")
			if (name == "k10temp" || name == "zenpower") && label != "" && label != "Tctl" && label != "Tdie" {
				continue
			}
			out = append(out, in)
		}
	}
	if len(out) > 0 {
		return out
	}
	zones, _ := filepath.Glob(root + "/class/thermal/thermal_zone*")
	var acpi []string
	for _, z := range zones {
		t := strings.ToLower(readTrim(z + "/type"))
		if t == "x86_pkg_temp" || strings.Contains(t, "cpu") {
			out = append(out, z+"/temp")
		}
		if t == "acpitz" && acpiCPUZone.MatchString(readTrim(z+"/device/path")) {
			acpi = append(acpi, z+"/temp")
		}
	}
	if len(out) > 0 {
		return out
	}
	return acpi
}

var acpiCPUZone = regexp.MustCompile(`^\\_TZ_\.TS\d[EP]$`)

func readTrim(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

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
	out, err := exec.CommandContext(ctx, smiPath, "--query-gpu="+fields, "--format=csv,noheader,nounits").Output()
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
