//go:build windows

package hostmon

import (
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Supported: Windows is read through its own calls, none of which needs an
// administrator — each CPU's times from ntdll, its clock from the
// performance counters, memory, the network adapters, the disks, the
// Wi-Fi signal — and the NVIDIA cards through nvidia-smi, as on Linux.
// Windows gives no CPU temperature without a driver of its own.
const Supported = true

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ntdll    = windows.NewLazySystemDLL("ntdll.dll")
	pdh      = windows.NewLazySystemDLL("pdh.dll")
	wlanapi  = windows.NewLazySystemDLL("wlanapi.dll")

	procGetActiveProcessorGroupCount     = kernel32.NewProc("GetActiveProcessorGroupCount")
	procGetLogicalProcessorInformationEx = kernel32.NewProc("GetLogicalProcessorInformationEx")
	procGlobalMemoryStatusEx             = kernel32.NewProc("GlobalMemoryStatusEx")
	procNtQuerySystemInformationEx       = ntdll.NewProc("NtQuerySystemInformationEx")
	procPdhOpenQueryW                    = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW            = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData              = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW     = pdh.NewProc("PdhGetFormattedCounterArrayW")
	procWlanOpenHandle                   = wlanapi.NewProc("WlanOpenHandle")
	procWlanQueryInterface               = wlanapi.NewProc("WlanQueryInterface")
	procWlanFreeMemory                   = wlanapi.NewProc("WlanFreeMemory")
)

func readHost() counters {
	c := counters{at: time.Now()}
	winTopoOnce.Do(func() { winTopo = readWindowsTopology() })
	if per := cpuTimes(winTopo.groups); per != nil {
		c.perCPU = per
		for _, v := range per {
			c.cpuBusy += v[0]
			c.cpuTotal += v[1]
		}
		c.cpuOK = true
	}
	c.freq = readClocks(winTopo.groups, len(winTopo.cpus))
	var m memoryStatusEx
	m.length = uint32(unsafe.Sizeof(m))
	if r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); r != 0 {
		c.memTotal, c.memAvail = m.totalPhys, m.availPhys
	}
	var ifaces []Iface
	c.net, ifaces, c.links = readAdapters()
	c.disk = readDisks()
	c.gpu = readGPU()
	host, _ := os.Hostname()
	c.info = Info{Host: host, Cores: runtime.NumCPU(), MemoryTotal: c.memTotal, GPU: c.gpu.name, GPUs: c.gpu.names, Net: names(c.net), Disks: names(c.disk),
		CPUs: winTopo.cpus, CPUKinds: winTopo.kinds, Cards: c.gpu.detailInfo, Ifaces: ifaces}
	return c
}

// quiet keeps a console program the daemon runs (nvidia-smi) from opening
// a window of its own.
func quiet(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

type memoryStatusEx struct {
	length, memoryLoad                   uint32
	totalPhys, availPhys                 uint64
	totalPageFile, availPageFile         uint64
	totalVirtual, availVirtual, availExt uint64
}

// ---- the processor ---------------------------------------------------------

// A PC with more than 64 logical processors has them in groups of up to
// 64; a CPU's number here is its place counting through the groups in
// order, which is how the performance counters' "group,number" instances
// and the groups' tick tables line up too.
type winTopology struct {
	topology
	groups []int // logical processors in each group
}

var winTopoOnce sync.Once
var winTopo winTopology

// readWindowsTopology reads each core's logical processors and efficiency
// class (GetLogicalProcessorInformationEx, RelationProcessorCore) and the
// processor's name from the registry. No figure gives a core's top clock
// with its boost, so max_mhz stays 0 and the page shows the clock alone.
func readWindowsTopology() winTopology {
	var t winTopology
	ng := 1
	if r, _, _ := procGetActiveProcessorGroupCount.Call(); r > 0 {
		ng = int(r)
	}
	base := make([]int, ng)
	n := 0
	for g := range ng {
		base[g] = n
		k := int(windows.GetActiveProcessorCount(uint16(g)))
		t.groups = append(t.groups, k)
		n += k
	}
	model := "CPU"
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil && cleanModel(s) != "" {
			model = cleanModel(s)
		}
		k.Close()
	}
	cores, classes := coreRecords(base)
	t.cpus = make([]CPU, n)
	for i := range t.cpus {
		t.cpus[i] = CPU{Core: i} // a processor no record names is a core of its own
	}
	var cls []int
	for _, c := range classes {
		cls = append(cls, c)
	}
	kinds, kind := classKinds(cls, model)
	t.kinds = kinds
	for core, cpus := range cores {
		for _, i := range cpus {
			if i < n {
				t.cpus[i] = CPU{Kind: kind[classes[core]], Core: core}
			}
		}
	}
	return t
}

// coreRecords reads one record a core: its logical processors, numbered
// through the groups, and its efficiency class.
func coreRecords(base []int) (cores [][]int, classes []int) {
	const relationProcessorCore = 0
	var size uint32
	procGetLogicalProcessorInformationEx.Call(relationProcessorCore, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	if r, _, _ := procGetLogicalProcessorInformationEx.Call(relationProcessorCore, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return nil, nil
	}
	le := binary.LittleEndian
	// SYSTEM_LOGICAL_PROCESSOR_INFORMATION_EX: Relationship, Size, then
	// PROCESSOR_RELATIONSHIP: Flags (8), EfficiencyClass (9), GroupCount
	// (30), and from 32 its GROUP_AFFINITY entries, 16 bytes each: a mask,
	// then the group
	for off := 0; off+8 <= int(size); {
		rel, sz := le.Uint32(buf[off:]), int(le.Uint32(buf[off+4:]))
		if sz <= 0 || off+sz > int(size) {
			break
		}
		if rel == relationProcessorCore && sz >= 48 {
			var cpus []int
			gc := int(le.Uint16(buf[off+30:]))
			for k := range gc {
				m := off + 32 + 16*k
				if m+16 > off+sz {
					break
				}
				mask, g := le.Uint64(buf[m:]), int(le.Uint16(buf[m+8:]))
				if g >= len(base) {
					continue
				}
				for bit := range 64 {
					if mask&(1<<bit) != 0 {
						cpus = append(cpus, base[g]+bit)
					}
				}
			}
			cores = append(cores, cpus)
			classes = append(classes, int(buf[off+9]))
		}
		off += sz
	}
	return cores, classes
}

// SYSTEM_PROCESSOR_PERFORMANCE_INFORMATION, in 100 ns: the kernel's time
// includes the idle time.
type procTimes struct {
	idle, kernel, user, dpc, interrupt int64
	count                              uint32
	_                                  uint32
}

// cpuTimes is each logical processor's busy and total time; a PC with more
// than one group is asked a group at a time.
func cpuTimes(groups []int) map[int][2]uint64 {
	per := map[int][2]uint64{}
	base := 0
	for g, n := range groups {
		if n == 0 {
			continue
		}
		buf := make([]procTimes, n)
		size := uint32(n) * uint32(unsafe.Sizeof(procTimes{}))
		var ret uint32
		if len(groups) == 1 {
			if err := windows.NtQuerySystemInformation(windows.SystemProcessorPerformanceInformation, unsafe.Pointer(&buf[0]), size, &ret); err != nil {
				return nil
			}
		} else {
			grp := uint16(g)
			if r, _, _ := procNtQuerySystemInformationEx.Call(uintptr(windows.SystemProcessorPerformanceInformation), uintptr(unsafe.Pointer(&grp)),
				unsafe.Sizeof(grp), uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&ret))); r != 0 {
				return nil
			}
		}
		got := min(int(ret/uint32(unsafe.Sizeof(procTimes{}))), n)
		for i := range got {
			p := buf[i]
			total := uint64(p.kernel + p.user)
			per[base+i] = [2]uint64{total - uint64(p.idle), total}
		}
		base += n
	}
	return per
}

// The clocks come from the performance counters, as Task Manager's speed
// does: "% Processor Performance" of "Processor Frequency" (the rated
// clock), past 100 with the boost. The first reading only starts the
// count.
var clocks struct {
	sync.Mutex
	tried       bool
	query       uintptr
	perf, rated uintptr
}

func readClocks(groups []int, n int) []int {
	clocks.Lock()
	defer clocks.Unlock()
	if !clocks.tried {
		clocks.tried = true
		if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&clocks.query))); r != 0 {
			clocks.query = 0
			return nil
		}
		add := func(path string, h *uintptr) bool {
			p, _ := windows.UTF16PtrFromString(path)
			r, _, _ := procPdhAddEnglishCounterW.Call(clocks.query, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(h)))
			return r == 0
		}
		if !add(`\Processor Information(*)\% Processor Performance`, &clocks.perf) || !add(`\Processor Information(*)\Processor Frequency`, &clocks.rated) {
			clocks.query = 0
			return nil
		}
	}
	if clocks.query == 0 {
		return nil
	}
	if r, _, _ := procPdhCollectQueryData.Call(clocks.query); r != 0 {
		return nil
	}
	perf, rated := pdhArray(clocks.perf), pdhArray(clocks.rated)
	if len(perf) == 0 {
		return nil
	}
	base := make([]int, len(groups))
	for g := 1; g < len(groups); g++ {
		base[g] = base[g-1] + groups[g-1]
	}
	out := make([]int, n)
	for name, p := range perf {
		g, i, ok := strings.Cut(name, ",")
		gi, err1 := strconv.Atoi(g)
		ii, err2 := strconv.Atoi(i)
		if !ok || err1 != nil || err2 != nil || gi >= len(base) || base[gi]+ii >= n {
			continue // "_Total", "0,_Total"
		}
		if r := rated[name]; r > 0 && p > 0 {
			out[base[gi]+ii] = int(r*p/100 + 0.5)
		}
	}
	return out
}

// pdhArray is a counter's value for each of its instances, by name.
func pdhArray(counter uintptr) map[string]float64 {
	const fmtDouble, noCap100, moreData = 0x200, 0x8000, 0x800007D2
	var size, count uint32
	r, _, _ := procPdhGetFormattedCounterArrayW.Call(counter, fmtDouble|noCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if r != moreData || size == 0 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ = procPdhGetFormattedCounterArrayW.Call(counter, fmtDouble|noCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil
	}
	// PDH_FMT_COUNTERVALUE_ITEM_W: the name's pointer, then the status and
	// the value
	type item struct {
		name   *uint16
		status uint32
		_      uint32
		value  float64
	}
	items := unsafe.Slice((*item)(unsafe.Pointer(&buf[0])), count)
	out := make(map[string]float64, count)
	for _, it := range items {
		if it.status <= 1 { // valid, or new
			out[windows.UTF16PtrToString(it.name)] = it.value
		}
	}
	runtime.KeepAlive(buf)
	return out
}

// ---- the network -----------------------------------------------------------

// readAdapters reads the physical network adapters: an Ethernet or Wi-Fi
// interface the system marks as hardware with a connector, and not one of
// the filter drivers stacked on it. Hyper-V's and VMware's virtual
// adapters, the network bridge, Tailscale, the WAN miniports and the
// Wi-Fi Direct adapters are none of those.
func readAdapters() (map[string][2]uint64, []Iface, map[string]link) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		return nil, nil, nil
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	const hardware, filter, connector = 1, 2, 4
	const ethernet, ieee80211, native80211 = 6, 71, 9
	net := map[string][2]uint64{}
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	found := map[string]*windows.MibIfRow2{}
	for i := range rows {
		r := &rows[i]
		f := r.InterfaceAndOperStatusFlags
		if f&hardware == 0 || f&filter != 0 || f&connector == 0 || (r.Type != ethernet && r.Type != ieee80211) {
			continue
		}
		name := windows.UTF16ToString(r.Alias[:])
		if name == "" {
			continue
		}
		net[name] = [2]uint64{r.InOctets, r.OutOctets}
		found[name] = r
	}
	ifaces := make([]Iface, 0, len(net))
	links := map[string]link{}
	for _, name := range names(net) {
		r := found[name]
		ifc := Iface{Name: name, Wireless: r.Type == ieee80211 || r.PhysicalMediumType == native80211,
			Model: adapterModel(r.InterfaceGuid, windows.UTF16ToString(r.Description[:]))}
		l := link{up: r.OperStatus == 1 && r.MediaConnectState == 1} // IfOperStatusUp, MediaConnectStateConnected
		if l.up && !ifc.Wireless && r.ReceiveLinkSpeed >= 1e6 {
			ifc.Speed = int(r.ReceiveLinkSpeed / 1e6)
		}
		if l.up && ifc.Wireless {
			l.signal = wifiSignal(r.InterfaceGuid)
		}
		ifaces = append(ifaces, ifc)
		links[name] = l
	}
	return net, ifaces, links
}

// adapterModel names an adapter by its driver's description and the
// device instance ID the registry keeps for its connection; it is kept
// by the adapter's GUID.
var adapterModels sync.Map

func adapterModel(guid windows.GUID, desc string) string {
	id := guid.String()
	if m, ok := adapterModels.Load(id); ok {
		return m.(string)
	}
	pnp := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Network\{4D36E972-E325-11CE-BFC1-08002BE10318}\`+id+`\Connection`, registry.QUERY_VALUE); err == nil {
		pnp, _, _ = k.GetStringValue("PnPInstanceId")
		k.Close()
	}
	m := windowsNICModel(desc, pnp)
	adapterModels.Store(id, m)
	return m
}

// wifiSignal is a Wi-Fi adapter's signal in dBm, from the WLAN service's
// own figure (wlan_intf_opcode_rssi); nil where the service is off or
// will not say.
var wlan struct {
	sync.Mutex
	tried  bool
	handle uintptr
}

func wifiSignal(guid windows.GUID) *float64 {
	wlan.Lock()
	defer wlan.Unlock()
	if !wlan.tried {
		wlan.tried = true
		var ver uint32
		if procWlanOpenHandle.Find() != nil {
			return nil
		}
		if r, _, _ := procWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&ver)), uintptr(unsafe.Pointer(&wlan.handle))); r != 0 {
			wlan.handle = 0
		}
	}
	if wlan.handle == 0 {
		return nil
	}
	const opcodeRSSI = 0x10000102
	var size uint32
	var data *int32 // the service's memory, a LONG
	if r, _, _ := procWlanQueryInterface.Call(wlan.handle, uintptr(unsafe.Pointer(&guid)), opcodeRSSI, 0,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), 0); r != 0 || data == nil {
		return nil
	}
	defer procWlanFreeMemory.Call(uintptr(unsafe.Pointer(data)))
	if size < 4 {
		return nil
	}
	v := *data
	if v >= 0 || v < -120 {
		return nil
	}
	return num(float64(v))
}

// ---- the disks -------------------------------------------------------------

// The physical disks, \\.\PhysicalDriveN, each asked for its byte counts
// (IOCTL_DISK_PERFORMANCE) through a handle that may neither read nor
// write it. A Storage Space, a mounted virtual disk and the like are
// left out: their bytes are their disks' too. Which drives there are is
// looked at again each minute.
var disks struct {
	sync.Mutex
	at   time.Time
	list []int
}

const (
	ioctlDiskPerformance = 0x70020
	ioctlStorageQuery    = 0x2d1400
	diskSearch           = 64
)

func readDisks() map[string][2]uint64 {
	disks.Lock()
	if disks.list == nil || time.Since(disks.at) > time.Minute {
		disks.at = time.Now()
		disks.list = []int{}
		for i := range diskSearch {
			if h := openDisk(i); h != windows.InvalidHandle {
				if physicalBus(h) {
					disks.list = append(disks.list, i)
				}
				windows.CloseHandle(h)
			}
		}
	}
	list := disks.list
	disks.Unlock()
	out := map[string][2]uint64{}
	for _, i := range list {
		h := openDisk(i)
		if h == windows.InvalidHandle {
			continue
		}
		var perf [88]byte // DISK_PERFORMANCE: bytes read, then bytes written
		var ret uint32
		if windows.DeviceIoControl(h, ioctlDiskPerformance, nil, 0, &perf[0], uint32(len(perf)), &ret, nil) == nil && ret >= 16 {
			out["PhysicalDrive"+strconv.Itoa(i)] = [2]uint64{binary.LittleEndian.Uint64(perf[0:]), binary.LittleEndian.Uint64(perf[8:])}
		}
		windows.CloseHandle(h)
	}
	return out
}

func openDisk(i int) windows.Handle {
	p, _ := windows.UTF16PtrFromString(`\\.\PhysicalDrive` + strconv.Itoa(i))
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return windows.InvalidHandle
	}
	return h
}

// physicalBus asks a disk's bus (STORAGE_DEVICE_DESCRIPTOR's BusType):
// not a virtual one (0x0E), a file-backed one (0x0F) or a Storage Space
// (0x10).
func physicalBus(h windows.Handle) bool {
	var query [12]byte // StorageDeviceProperty, PropertyStandardQuery
	var desc [1024]byte
	var ret uint32
	if windows.DeviceIoControl(h, ioctlStorageQuery, &query[0], uint32(len(query)), &desc[0], uint32(len(desc)), &ret, nil) != nil || ret < 32 {
		return true // a disk that will not say is counted
	}
	switch binary.LittleEndian.Uint32(desc[28:]) {
	case 0x0E, 0x0F, 0x10:
		return false
	}
	return true
}
