//go:build darwin

package hostmon

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/sysctl.h>
#include <sys/ioctl.h>
#include <net/if.h>
#include <net/if_media.h>
#include <net/route.h>
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <mach/processor_info.h>
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>

// One send right to the host port, kept: mach_host_self() hands out a new
// one each call.
static mach_port_t hostPort(void) {
	static mach_port_t host = MACH_PORT_NULL;
	if (host == MACH_PORT_NULL) host = mach_host_self();
	return host;
}

// cpuTicks fills each CPU's busy and total ticks, up to max; it returns how
// many CPUs it filled, or -1.
static int cpuTicks(unsigned long long *busy, unsigned long long *total, int max) {
	natural_t n = 0;
	processor_info_array_t info;
	mach_msg_type_number_t cnt;
	if (host_processor_info(hostPort(), PROCESSOR_CPU_LOAD_INFO, &n, &info, &cnt) != KERN_SUCCESS) return -1;
	processor_cpu_load_info_t load = (processor_cpu_load_info_t)info;
	int m = n < (natural_t)max ? (int)n : max;
	for (int i = 0; i < m; i++) {
		unsigned int *t = load[i].cpu_ticks;
		busy[i] = (unsigned long long)t[CPU_STATE_USER] + t[CPU_STATE_SYSTEM] + t[CPU_STATE_NICE];
		total[i] = busy[i] + t[CPU_STATE_IDLE];
	}
	vm_deallocate(mach_task_self(), (vm_address_t)info, cnt * sizeof(integer_t));
	return m;
}

// memUsed is memory in use as Activity Monitor counts it: app memory
// (anonymous pages, less what may be purged), wired and compressed.
static int memUsed(unsigned long long *used) {
	vm_statistics64_data_t vm;
	mach_msg_type_number_t cnt = HOST_VM_INFO64_COUNT;
	if (host_statistics64(hostPort(), HOST_VM_INFO64, (host_info64_t)&vm, &cnt) != KERN_SUCCESS) return -1;
	vm_size_t page = 0;
	if (host_page_size(hostPort(), &page) != KERN_SUCCESS || page == 0) return -1;
	unsigned long long app = vm.internal_page_count > vm.purgeable_count ? vm.internal_page_count - vm.purgeable_count : 0;
	*used = (app + vm.wire_count + vm.compressor_page_count) * (unsigned long long)page;
	return 0;
}

// One interface's counters, 64 bits wide (if_data's own are 32 and wrap
// every 4 GB).
typedef struct {
	char name[IFNAMSIZ];
	unsigned long long in, out, baud;
} netCount;

static int netCounts(netCount *out, int max) {
	int mib[6] = {CTL_NET, PF_ROUTE, 0, 0, NET_RT_IFLIST2, 0};
	size_t len = 0;
	if (sysctl(mib, 6, NULL, &len, NULL, 0) < 0) return -1;
	char *buf = malloc(len);
	if (!buf) return -1;
	if (sysctl(mib, 6, buf, &len, NULL, 0) < 0) { free(buf); return -1; }
	int n = 0;
	for (char *p = buf; p < buf + len && n < max;) {
		struct if_msghdr *ifm = (struct if_msghdr *)p;
		if (ifm->ifm_msglen == 0) break;
		if (ifm->ifm_type == RTM_IFINFO2) {
			struct if_msghdr2 *m2 = (struct if_msghdr2 *)p;
			if (if_indextoname(m2->ifm_index, out[n].name)) {
				out[n].in = m2->ifm_data.ifi_ibytes;
				out[n].out = m2->ifm_data.ifi_obytes;
				out[n].baud = m2->ifm_data.ifi_baudrate;
				n++;
			}
		}
		p += ifm->ifm_msglen;
	}
	free(buf);
	return n;
}

// linkActive says whether the interface's medium has a link: 1, 0, or -1
// where it cannot tell.
static int linkActive(const char *name) {
	int s = socket(AF_INET, SOCK_DGRAM, 0);
	if (s < 0) return -1;
	struct ifmediareq ifmr;
	memset(&ifmr, 0, sizeof ifmr);
	strlcpy(ifmr.ifm_name, name, sizeof ifmr.ifm_name);
	int r = ioctl(s, SIOCGIFMEDIA, &ifmr);
	close(s);
	if (r < 0 || !(ifmr.ifm_status & IFM_AVALID)) return -1;
	return (ifmr.ifm_status & IFM_ACTIVE) ? 1 : 0;
}

static int cfNumber(CFDictionaryRef d, const char *key, long long *v) {
	CFStringRef k = CFStringCreateWithCString(kCFAllocatorDefault, key, kCFStringEncodingUTF8);
	CFTypeRef n = CFDictionaryGetValue(d, k);
	CFRelease(k);
	return n && CFGetTypeID(n) == CFNumberGetTypeID() && CFNumberGetValue((CFNumberRef)n, kCFNumberLongLongType, v);
}

// gpuRead is the Apple GPU's share of time at work, from its
// accelerator's PerformanceStatistics, and its model ("Apple M1").
static int gpuRead(long long *util, char *model, int modelLen) {
	io_iterator_t it;
	if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOAccelerator"), &it) != KERN_SUCCESS) return -1;
	io_object_t o;
	int found = 0;
	while (!found && (o = IOIteratorNext(it))) {
		CFMutableDictionaryRef props = NULL;
		if (IORegistryEntryCreateCFProperties(o, &props, kCFAllocatorDefault, 0) == KERN_SUCCESS && props) {
			CFTypeRef ps = CFDictionaryGetValue(props, CFSTR("PerformanceStatistics"));
			if (ps && CFGetTypeID(ps) == CFDictionaryGetTypeID() && cfNumber((CFDictionaryRef)ps, "Device Utilization %", util)) {
				found = 1;
				CFTypeRef m = CFDictionaryGetValue(props, CFSTR("model"));
				if (m && CFGetTypeID(m) == CFStringGetTypeID()) CFStringGetCString((CFStringRef)m, model, modelLen, kCFStringEncodingUTF8);
			}
			CFRelease(props);
		}
		IOObjectRelease(o);
	}
	IOObjectRelease(it);
	return found ? 0 : -1;
}

static int regString(io_registry_entry_t e, const char *key, char *out, int n) {
	CFStringRef k = CFStringCreateWithCString(kCFAllocatorDefault, key, kCFStringEncodingUTF8);
	CFTypeRef v = IORegistryEntryCreateCFProperty(e, k, kCFAllocatorDefault, 0);
	CFRelease(k);
	int ok = v && CFGetTypeID(v) == CFStringGetTypeID() && CFStringGetCString((CFStringRef)v, out, n, kCFStringEncodingUTF8);
	if (v) CFRelease(v);
	return ok;
}

// regNumber reads a number, or a PCI ID: data whose first two bytes are
// the ID, low byte first. It is -1 where the entry has neither.
static long long regNumber(io_registry_entry_t e, const char *key) {
	CFStringRef k = CFStringCreateWithCString(kCFAllocatorDefault, key, kCFStringEncodingUTF8);
	CFTypeRef v = IORegistryEntryCreateCFProperty(e, k, kCFAllocatorDefault, 0);
	CFRelease(k);
	long long x = -1;
	if (v && CFGetTypeID(v) == CFNumberGetTypeID()) CFNumberGetValue((CFNumberRef)v, kCFNumberLongLongType, &x);
	if (v && CFGetTypeID(v) == CFDataGetTypeID() && CFDataGetLength((CFDataRef)v) >= 2) {
		const UInt8 *b = CFDataGetBytePtr((CFDataRef)v);
		x = b[0] | b[1] << 8;
	}
	if (v) CFRelease(v);
	return x;
}

// nicInfo walks up from a network interface to the device under it. On
// the way, a driver may name its chip (IOModel "AQC113"; Broadcom's Wi-Fi
// a ChipSet, 4378). A PCI device gives its vendor ID (bus 1); a USB one
// its maker and product (bus 2). It returns 0 where it finds neither.
static int nicInfo(const char *bsd, int *vendor, char *model, int n, char *maker, int mn) {
	io_service_t s = IOServiceGetMatchingService(kIOMainPortDefault, IOBSDNameMatching(kIOMainPortDefault, 0, bsd));
	io_registry_entry_t e = s;
	int bus = 0;
	long long chip = -1;
	*vendor = 0; model[0] = maker[0] = 0;
	for (int depth = 0; e && depth < 16 && !bus; depth++) {
		if (!model[0]) regString(e, "IOModel", model, n);
		if (chip < 0) chip = regNumber(e, "ChipSet");
		if (IOObjectConformsTo(e, "IOPCIDevice")) {
			long long v = regNumber(e, "vendor-id");
			*vendor = v > 0 ? (int)v : 0;
			bus = 1;
		} else if (IOObjectConformsTo(e, "IOUSBHostDevice")) {
			regString(e, "USB Product Name", model, n);
			regString(e, "USB Vendor Name", maker, mn);
			bus = 2;
		}
		io_registry_entry_t p = 0;
		if (!bus && IORegistryEntryGetParentEntry(e, kIOServicePlane, &p) != KERN_SUCCESS) p = 0;
		IOObjectRelease(e);
		e = bus ? 0 : p;
	}
	if (e) IOObjectRelease(e);
	if (bus == 1 && !model[0] && chip > 0 && *vendor == 0x14e4) snprintf(model, n, "BCM%lld", chip);
	return bus;
}

// The chip's temperature sensors, through the HID event system: a
// private interface of IOKit's that monitoring tools have long used, and
// one that needs no root. An M1 names its CPU clusters' sensors "pACC MTR
// Temp Sensor2" and "eACC MTR …", its GPU's "GPU MTR …", and its dies'
// "PMU tdie1" and on.
typedef struct __IOHIDEventSystemClient *IOHIDEventSystemClientRef;
typedef struct __IOHIDServiceClient *IOHIDServiceClientRef;
typedef struct __IOHIDEvent *IOHIDEventRef;
IOHIDEventSystemClientRef IOHIDEventSystemClientCreate(CFAllocatorRef);
int IOHIDEventSystemClientSetMatching(IOHIDEventSystemClientRef, CFDictionaryRef);
CFArrayRef IOHIDEventSystemClientCopyServices(IOHIDEventSystemClientRef);
CFTypeRef IOHIDServiceClientCopyProperty(IOHIDServiceClientRef, CFStringRef);
IOHIDEventRef IOHIDServiceClientCopyEvent(IOHIDServiceClientRef, int64_t, int32_t, int64_t);
double IOHIDEventGetFloatValue(IOHIDEventRef, int32_t);
#define kHIDTemperature 15

// hidTemps gives the hottest CPU cluster sensor (else the hottest die
// sensor) and the hottest GPU sensor; a figure it has none for is -1000.
static void hidTemps(double *cpu, double *gpu) {
	static CFArrayRef services = NULL;
	*cpu = *gpu = -1000;
	if (!services) {
		int page = 0xff00, usage = 5;
		CFNumberRef p = CFNumberCreate(NULL, kCFNumberIntType, &page), u = CFNumberCreate(NULL, kCFNumberIntType, &usage);
		const void *k[] = {CFSTR("PrimaryUsagePage"), CFSTR("PrimaryUsage")}, *v[] = {p, u};
		CFDictionaryRef match = CFDictionaryCreate(NULL, k, v, 2, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
		IOHIDEventSystemClientRef sys = IOHIDEventSystemClientCreate(kCFAllocatorDefault);
		if (sys) {
			IOHIDEventSystemClientSetMatching(sys, match);
			services = IOHIDEventSystemClientCopyServices(sys); // kept, with the client, for the life of the daemon
		}
		CFRelease(match); CFRelease(p); CFRelease(u);
		if (!services) return;
	}
	double die = -1000;
	for (CFIndex i = 0; i < CFArrayGetCount(services); i++) {
		IOHIDServiceClientRef s = (IOHIDServiceClientRef)CFArrayGetValueAtIndex(services, i);
		CFTypeRef name = IOHIDServiceClientCopyProperty(s, CFSTR("Product"));
		char buf[96] = "";
		if (name) {
			if (CFGetTypeID(name) == CFStringGetTypeID()) CFStringGetCString((CFStringRef)name, buf, sizeof buf, kCFStringEncodingUTF8);
			CFRelease(name);
		}
		int isCPU = strstr(buf, "ACC MTR") != NULL, isGPU = strstr(buf, "GPU MTR") != NULL, isDie = strstr(buf, "tdie") != NULL;
		if (!isCPU && !isGPU && !isDie) continue;
		IOHIDEventRef ev = IOHIDServiceClientCopyEvent(s, kHIDTemperature, 0, 0);
		if (!ev) continue;
		double t = IOHIDEventGetFloatValue(ev, kHIDTemperature << 16);
		CFRelease(ev);
		if (t <= 0 || t > 150) continue; // a sensor that is off reads -22
		if (isCPU && t > *cpu) *cpu = t;
		if (isGPU && t > *gpu) *gpu = t;
		if (isDie && t > die) die = t;
	}
	if (*cpu < -999) *cpu = die;
}

typedef struct {
	char name[32];
	unsigned long long rd, wr;
} diskCount;

// diskCounts reads each block storage driver's bytes read and written,
// leaving out disk images (their device's Physical Interconnect is
// "Virtual Interface"): their bytes are the real disk's too.
static int diskCounts(diskCount *out, int max) {
	io_iterator_t it;
	if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOBlockStorageDriver"), &it) != KERN_SUCCESS) return -1;
	io_object_t o;
	int n = 0;
	while (n < max && (o = IOIteratorNext(it))) {
		int virt = 0;
		io_registry_entry_t parent;
		if (IORegistryEntryGetParentEntry(o, kIOServicePlane, &parent) == KERN_SUCCESS) {
			CFTypeRef pc = IORegistryEntryCreateCFProperty(parent, CFSTR("Protocol Characteristics"), kCFAllocatorDefault, 0);
			if (pc) {
				if (CFGetTypeID(pc) == CFDictionaryGetTypeID()) {
					CFTypeRef pi = CFDictionaryGetValue((CFDictionaryRef)pc, CFSTR("Physical Interconnect"));
					if (pi && CFGetTypeID(pi) == CFStringGetTypeID() &&
						CFStringCompare((CFStringRef)pi, CFSTR("Virtual Interface"), 0) == kCFCompareEqualTo) virt = 1;
				}
				CFRelease(pc);
			}
			IOObjectRelease(parent);
		}
		CFTypeRef st = virt ? NULL : IORegistryEntryCreateCFProperty(o, CFSTR("Statistics"), kCFAllocatorDefault, 0);
		if (st) {
			long long rd = 0, wr = 0;
			if (CFGetTypeID(st) == CFDictionaryGetTypeID() &&
				cfNumber((CFDictionaryRef)st, "Bytes (Read)", &rd) && cfNumber((CFDictionaryRef)st, "Bytes (Write)", &wr)) {
				out[n].rd = rd;
				out[n].wr = wr;
				out[n].name[0] = 0;
				CFTypeRef bsd = IORegistryEntrySearchCFProperty(o, kIOServicePlane, CFSTR("BSD Name"), kCFAllocatorDefault, kIORegistryIterateRecursively);
				if (bsd) {
					if (CFGetTypeID(bsd) == CFStringGetTypeID()) CFStringGetCString((CFStringRef)bsd, out[n].name, sizeof out[n].name, kCFStringEncodingUTF8);
					CFRelease(bsd);
				}
				if (!out[n].name[0]) snprintf(out[n].name, sizeof out[n].name, "disk%d", n);
				n++;
			}
			CFRelease(st);
		}
		IOObjectRelease(o);
	}
	IOObjectRelease(it);
	return n;
}
*/
import "C"

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Supported: macOS is read through Mach, sysctl and IOKit. What it keeps
// behind root — power, clocks, the Wi-Fi signal — it gives no figure for.
const Supported = true

const maxCPUs = 256

func readHost() counters {
	c := counters{at: time.Now()}
	darwinTopoOnce.Do(func() { darwinTopo = readDarwinTopology() })
	var busy, total [maxCPUs]C.ulonglong
	if n := int(C.cpuTicks(&busy[0], &total[0], maxCPUs)); n > 0 {
		c.perCPU = map[int][2]uint64{}
		for i := range n {
			b, t := uint64(busy[i]), uint64(total[i])
			c.perCPU[i] = [2]uint64{b, t}
			c.cpuBusy += b
			c.cpuTotal += t
		}
		c.cpuOK = true
	}
	if total, err := unix.SysctlUint64("hw.memsize"); err == nil {
		var used C.ulonglong
		c.memTotal = total
		if C.memUsed(&used) == 0 {
			c.memAvail = total - min(uint64(used), total)
		}
	}
	// the network: the physical interfaces networksetup names
	phys := physicalPorts()
	var nc [64]C.netCount
	c.net = map[string][2]uint64{}
	for i := range int(C.netCounts(&nc[0], 64)) {
		name := C.GoString(&nc[i].name[0])
		if _, ok := phys[name]; ok {
			c.net[name] = [2]uint64{uint64(nc[i].in), uint64(nc[i].out)}
			phys[name] = port{wireless: phys[name].wireless, baud: uint64(nc[i].baud)}
		}
	}
	ifaces := make([]Iface, 0, len(c.net))
	c.links = map[string]link{}
	for _, name := range names(c.net) {
		p := phys[name]
		cs := C.CString(name)
		up := C.linkActive(cs) == 1
		C.free(unsafe.Pointer(cs))
		ifc := Iface{Name: name, Wireless: p.wireless, Model: darwinNICModel(name)}
		if up && !p.wireless && p.baud >= 1e6 {
			ifc.Speed = int(p.baud / 1e6)
		}
		ifaces = append(ifaces, ifc)
		c.links[name] = link{up: up}
	}
	// the disks, not their images
	var dc [64]C.diskCount
	c.disk = map[string][2]uint64{}
	for i := range int(C.diskCounts(&dc[0], 64)) {
		c.disk[C.GoString(&dc[i].name[0])] = [2]uint64{uint64(dc[i].rd), uint64(dc[i].wr)}
	}
	// the chip's temperatures
	var cpuT, gpuT C.double
	C.hidTemps(&cpuT, &gpuT)
	if cpuT > -999 {
		c.cpuTemp = num(float64(cpuT))
	}
	// the GPU, which shares the machine's memory
	var util C.longlong
	var model [128]C.char
	if C.gpuRead(&util, &model[0], C.int(len(model))) == 0 {
		name := C.GoString(&model[0])
		if name == "" {
			name = "Apple GPU"
		} else if !strings.Contains(strings.ToLower(name), "gpu") {
			name += " GPU"
		}
		u := num(float64(util))
		var temp *float64
		if gpuT > -999 {
			temp = num(float64(gpuT))
		}
		c.gpu = gpuReading{name: name, util: u, temp: temp, detail: []Card{{Util: u, Temp: temp}}, detailInfo: []CardInfo{{Name: name}}}
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	// the load average, and when the machine started
	var load [3]C.double
	if C.getloadavg(&load[0], 3) == 3 {
		c.load = []float64{float64(load[0]), float64(load[1]), float64(load[2])}
	}
	var boot int64
	if tv, err := unix.SysctlTimeval("kern.boottime"); err == nil {
		boot = int64(tv.Sec)
	}
	c.info = Info{Host: host, Cores: runtime.NumCPU(), MemoryTotal: c.memTotal, GPU: c.gpu.name, Net: names(c.net), Disks: names(c.disk),
		Boot: boot, CPUs: darwinTopo.cpus, CPUKinds: darwinTopo.kinds, Cards: c.gpu.detailInfo, Ifaces: ifaces, LoadAvg: c.load != nil}
	return c
}

// The models are kept by interface: what sits under en0 is the machine's
// own, and an adapter plugged in gets a name of its own.
var (
	nicMu     sync.Mutex
	nicModels = map[string]string{}
)

func darwinNICModel(name string) string {
	nicMu.Lock()
	defer nicMu.Unlock()
	if m, ok := nicModels[name]; ok {
		return m
	}
	var vendor C.int
	var model, maker [128]C.char
	cs := C.CString(name)
	bus := C.nicInfo(cs, &vendor, &model[0], C.int(len(model)), &maker[0], C.int(len(maker)))
	C.free(unsafe.Pointer(cs))
	m := ""
	switch bus {
	case 1:
		m = nicModel(uint16(vendor), "", C.GoString(&model[0]))
	case 2:
		m = usbModel(C.GoString(&maker[0]), C.GoString(&model[0]))
	}
	nicModels[name] = m
	return m
}

var darwinTopoOnce sync.Once
var darwinTopo topology

// readDarwinTopology numbers an Apple chip's cores by performance level:
// the kernel lists the least performant level first (on the M1, CPUs 0–3
// are its efficiency cores, 4–7 its performance cores), and each level
// names itself (hw.perflevelN.name, 0 the most performant). Each CPU is a
// core of its own. An Intel Mac has one level and no names: its model.
func readDarwinTopology() topology {
	var t topology
	levels, err := unix.SysctlUint32("hw.nperflevels")
	if err != nil || levels == 0 {
		n := runtime.NumCPU()
		brand, _ := unix.Sysctl("machdep.cpu.brand_string")
		t.kinds = []string{cleanModelDarwin(brand)}
		for i := range n {
			t.cpus = append(t.cpus, CPU{Kind: 0, Core: i})
		}
		return t
	}
	for l := range int(levels) {
		name, _ := unix.Sysctl("hw.perflevel" + strconv.Itoa(l) + ".name")
		if name == "" {
			name = "Level " + strconv.Itoa(l)
		}
		t.kinds = append(t.kinds, name)
	}
	for l := int(levels) - 1; l >= 0; l-- {
		n, _ := unix.SysctlUint32("hw.perflevel" + strconv.Itoa(l) + ".logicalcpu")
		for range int(n) {
			i := len(t.cpus)
			t.cpus = append(t.cpus, CPU{Kind: l, Core: i})
		}
	}
	return t
}

func cleanModelDarwin(s string) string {
	for _, m := range []string{"(R)", "(TM)", " CPU"} {
		s = strings.ReplaceAll(s, m, "")
	}
	if i := strings.Index(s, " @ "); i > 0 {
		s = s[:i]
	}
	if s = strings.Join(strings.Fields(s), " "); s == "" {
		return "CPU"
	}
	return s
}

// port is a physical interface as networksetup names it.
type port struct {
	wireless bool
	baud     uint64
}

var portsMu sync.Mutex
var ports map[string]port
var portsAt time.Time

// physicalPorts asks networksetup for the hardware ports, once a minute
// (an adapter plugged in later turns up): Ethernet and Wi-Fi and the
// like, but not Thunderbolt's bridge and ports, nor the virtual adapters
// macOS lists as "Ethernet Adapter (en4)".
func physicalPorts() map[string]port {
	portsMu.Lock()
	defer portsMu.Unlock()
	if ports == nil || time.Since(portsAt) > time.Minute {
		portsAt = time.Now()
		if out, err := exec.Command("/usr/sbin/networksetup", "-listallhardwareports").Output(); err == nil {
			ports = parseHardwarePorts(string(out))
		} else if ports == nil {
			ports = map[string]port{}
		}
	}
	cp := make(map[string]port, len(ports))
	for k, v := range ports {
		cp[k] = v
	}
	return cp
}

var virtualAdapter = regexp.MustCompile(`^Ethernet Adapter \(en\d+\)$`)

// parseHardwarePorts reads networksetup's "Hardware Port: …" and "Device:
// …" lines.
func parseHardwarePorts(s string) map[string]port {
	out := map[string]port{}
	var name string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
			name = strings.TrimSpace(v)
			continue
		}
		dev, ok := strings.CutPrefix(line, "Device: ")
		if !ok || name == "" {
			continue
		}
		dev = strings.TrimSpace(dev)
		if strings.HasPrefix(name, "Thunderbolt") || strings.HasPrefix(dev, "bridge") || virtualAdapter.MatchString(name) {
			name = ""
			continue
		}
		out[dev] = port{wireless: name == "Wi-Fi" || name == "AirPort"}
		name = ""
	}
	return out
}
