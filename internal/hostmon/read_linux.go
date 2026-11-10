//go:build linux

package hostmon

import (
	"bufio"
	"bytes"
	"fmt"
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
	"time"

	"exe/internal/hostinfo"
)

// Supported: Linux reads all of it from /proc, /sys and nvidia-smi.
const Supported = true

func readHost() counters {
	c := counters{at: time.Now()}
	var boot int64
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		c.cpuBusy, c.cpuTotal, c.cpuOK, c.perCPU = parseStat(bytes.NewReader(b))
		boot = parseBtime(b)
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		c.load = parseLoadavg(string(b))
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
		Boot: boot, CPUs: topo.cpus, CPUKinds: topo.kinds, Cards: c.gpu.detailInfo, Ifaces: ifaces, LoadAvg: c.load != nil}
	return c
}

// parseBtime reads /proc/stat's "btime" line: when the machine started, in
// unix seconds. The kernel works it out from the clock at each read, so a
// clock set right after the start moves it too.
func parseBtime(stat []byte) int64 {
	for line := range bytes.Lines(stat) {
		if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			n, _ := strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
			return n
		}
	}
	return 0
}

// parseLoadavg reads /proc/loadavg's first three fields: the load average
// over the last 1, 5 and 15 minutes.
func parseLoadavg(s string) []float64 {
	f := strings.Fields(s)
	if len(f) < 3 {
		return nil
	}
	out := make([]float64, 3)
	for i := range out {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return nil
		}
		out[i] = v
	}
	return out
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

// The machine's logical CPUs, read once: which kind of core each is and
// which physical core it is a thread of.
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
		ifc.Model = linuxNICModel(dir)
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

// The models are kept by the device an interface sits on, as finding one
// reads pci.ids, a megabyte and a half; a device that comes back under
// another name keeps its model.
var (
	nicMu     sync.Mutex
	nicModels = map[string]string{}
	pciIDs    = []string{"/usr/share/misc/pci.ids", "/usr/share/hwdata/pci.ids", "/usr/share/pci.ids"}
)

func linuxNICModel(dir string) string {
	dev, err := filepath.EvalSymlinks(dir + "/device")
	if err != nil {
		return ""
	}
	nicMu.Lock()
	defer nicMu.Unlock()
	m, ok := nicModels[dev]
	if !ok {
		m = readNICModel(dev)
		nicModels[dev] = m
	}
	return m
}

// readNICModel names the device under an interface: a PCI one by its IDs
// in pci.ids (or its driver, for a chip newer than the file), a USB one by
// what the adapter says of itself. A SoC's own port has neither: "".
func readNICModel(dev string) string {
	sub, _ := filepath.EvalSymlinks(dev + "/subsystem")
	switch filepath.Base(sub) {
	case "pci":
		v, err1 := strconv.ParseUint(strings.TrimPrefix(readTrim(dev+"/vendor"), "0x"), 16, 16)
		d, err2 := strconv.ParseUint(strings.TrimPrefix(readTrim(dev+"/device"), "0x"), 16, 16)
		if err1 != nil || err2 != nil {
			return ""
		}
		var vendorName, deviceName string
		for _, path := range pciIDs {
			if vendorName, deviceName = pciName(path, uint16(v), uint16(d)); vendorName != "" {
				break
			}
		}
		if deviceName == "" {
			driver, _ := filepath.EvalSymlinks(dev + "/driver")
			deviceName = driverChip(filepath.Base(driver), uint16(v), uint16(d))
		}
		return nicModel(uint16(v), vendorName, deviceName)
	case "usb":
		// the link names the adapter's interface; the device above it
		// carries the strings
		up := dev
		if _, err := os.Stat(up + "/idVendor"); err != nil {
			up = filepath.Dir(dev)
		}
		return usbModel(readTrim(up+"/manufacturer"), readTrim(up+"/product"))
	}
	return ""
}

// pciName looks a vendor and device up in a pci.ids file: a vendor line
// "10ec  Realtek Semiconductor Co., Ltd.", its devices under it, each
// "\t8125  RTL8125 2.5GbE Controller". The device classes at the file's
// end ("C 02  Network controller") are not looked in.
func pciName(path string, vendor, device uint16) (vendorName, deviceName string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	vp, dp := fmt.Sprintf("%04x  ", vendor), fmt.Sprintf("\t%04x  ", device)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] != '\t' {
			if vendorName != "" || strings.HasPrefix(line, "C ") {
				break
			}
			if strings.HasPrefix(line, vp) {
				vendorName = strings.TrimSpace(line[len(vp):])
			}
			continue
		}
		if vendorName != "" && strings.HasPrefix(line, dp) {
			deviceName = strings.TrimSpace(line[len(dp):])
			break
		}
	}
	return vendorName, deviceName
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

// quiet: nothing to hide on Linux.
func quiet(*exec.Cmd) {}
