// Package hostmon samples this machine's activity — CPU, memory, GPU,
// network and disk — every couple of seconds for the desktop's Control
// Strip, and each core and each graphics card on its own for the Activity
// Monitor. It samples only while someone is reading: the first read starts
// it, and it stops a while after the last one, keeping what it has.
package hostmon

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Sample is one moment's activity. Rates are bytes a second since the
// sample before; a figure the host cannot give is nil.
type Sample struct {
	T         int64    `json:"t"`              // unix milliseconds
	CPU       *float64 `json:"cpu"`            // percent of all cores busy
	CPUTemp   *float64 `json:"cpu_temp"`       // °C, the hottest CPU sensor
	Mem       *float64 `json:"mem"`            // percent of memory in use (total − available)
	MemUsed   uint64   `json:"mem_used"`       // bytes
	GPU       *float64 `json:"gpu"`            // percent utilisation, all GPUs averaged
	GPUTemp   *float64 `json:"gpu_temp"`       // °C, the hottest GPU
	GPUPower  *float64 `json:"gpu_power"`      // watts, all GPUs
	GPUs      []GPU    `json:"gpus,omitempty"` // each card, on a host with more than one
	NetIn     *float64 `json:"net_in"`         // over physical interfaces
	NetOut    *float64 `json:"net_out"`
	DiskRead  *float64 `json:"disk_read"` // over physical disks
	DiskWrite *float64 `json:"disk_write"`

	// The detail, sent only to a reader that asks for it (ReadDetail): the
	// Activity Monitor's, not the Control Strip's.
	Cores []int   `json:"cores,omitempty"` // each logical CPU's percent busy, in Info.CPUs' order; -1 for one offline
	Freq  []int   `json:"freq,omitempty"`  // each one's clock in MHz, 0 where the kernel gives none
	Cards []Card  `json:"cards,omitempty"` // each graphics card, one or more
	Nets  []NetIf `json:"nets,omitempty"`  // each physical network interface, in Info.Ifaces' order

	// A history row (ReadHistory) is a slot's averages: N is how many
	// samples it holds, Peak their highest figures, laid out the same.
	N    int     `json:"n,omitempty"`
	Peak *Sample `json:"peak,omitempty"`
}

// NetIf is one network interface's moment: bytes a second each way since
// the sample before, and its link.
type NetIf struct {
	In     *float64 `json:"in"`
	Out    *float64 `json:"out"`
	Up     bool     `json:"up"`               // the link is up
	Signal *float64 `json:"signal,omitempty"` // dBm, a wireless link's
}

// Iface is a physical network interface: a wired port or a wireless one.
type Iface struct {
	Name     string `json:"name"`
	Wireless bool   `json:"wireless"`
	Speed    int    `json:"speed"`           // Mb/s while a wired link is up; 0 where the kernel gives none
	Model    string `json:"model,omitempty"` // its controller, "Intel I219-LM"; "" where the system names none
}

// GPU is one card's figures, in nvidia-smi's order (its index).
type GPU struct {
	Util  *float64 `json:"util"`  // percent
	Temp  *float64 `json:"temp"`  // °C
	Power *float64 `json:"power"` // watts
}

// Card is one graphics card's reading of the moment. A figure the card
// does not give is nil: the GB10 shares the host's memory and reports none
// of its own, nor a fan.
type Card struct {
	Util    *float64 `json:"util"`     // percent of the time the GPU was at work
	MemUtil *float64 `json:"mem_util"` // percent of the time its memory was read or written
	MemUsed *float64 `json:"mem_used"` // bytes
	Temp    *float64 `json:"temp"`     // °C
	Power   *float64 `json:"power"`    // watts
	Fan     *float64 `json:"fan"`      // percent of the fan's top speed
	Clock   *float64 `json:"clock"`    // MHz, the graphics clock
	Enc     *float64 `json:"enc"`      // percent, the video encoder
	Dec     *float64 `json:"dec"`      // percent, the video decoder
	PState  string   `json:"pstate,omitempty"`
}

// CardInfo is what one graphics card is: its name, its fixed figures and
// which of the changing ones it gives, so a window can be laid out before
// its first sample.
type CardInfo struct {
	Name       string   `json:"name"`
	MemTotal   *float64 `json:"mem_total"`   // bytes; nil where the card shares the host's memory
	PowerLimit *float64 `json:"power_limit"` // watts
	ClockMax   *float64 `json:"clock_max"`   // MHz
	Fan        bool     `json:"fan"`         // it reports a fan's speed
	Video      bool     `json:"video"`       // it reports its video encoder and decoder
}

// CPU is one logical CPU as the kernel numbers them.
type CPU struct {
	Kind   int `json:"kind"`    // index into Info.CPUKinds
	Core   int `json:"core"`    // its physical core: threads of one core share the number
	MaxMHz int `json:"max_mhz"` // 0 where the kernel gives none
}

// Info names what the samples cover.
type Info struct {
	Host        string   `json:"host"` // the machine's hostname
	Cores       int      `json:"cores"`
	MemoryTotal uint64   `json:"memory_total"`
	GPU         string   `json:"gpu"`            // "" without one; "2 × …" for two of a kind
	GPUs        []string `json:"gpus,omitempty"` // each card's name, on a host with more than one
	Net         []string `json:"net"`
	Disks       []string `json:"disks"`

	// The detail (ReadDetail): each logical CPU, the kinds of core they
	// are ("Cortex-X925", "Intel Xeon w9-3475X"), and each graphics card.
	CPUs     []CPU      `json:"cpus,omitempty"`
	CPUKinds []string   `json:"cpu_kinds,omitempty"`
	Cards    []CardInfo `json:"cards,omitempty"`
	Ifaces   []Iface    `json:"ifaces,omitempty"` // the network interfaces of Net, as Nets gives them
}

// counters is one raw read of the host: running totals, from which two
// reads a moment apart make a Sample.
type counters struct {
	at                 time.Time
	cpuBusy, cpuTotal  uint64
	cpuOK              bool
	cpuTemp            *float64
	perCPU             map[int][2]uint64 // busy, total jiffies of each logical CPU
	freq               []int             // MHz of each, in topology order
	links              map[string]link   // each interface's link, by name
	memTotal, memAvail uint64
	net, disk          map[string][2]uint64 // in/out, read/written bytes per device
	gpu                gpuReading
	info               Info
}

// link is an interface's state at one reading.
type link struct {
	up     bool
	signal *float64
}

type gpuReading struct {
	name              string     // the cards' model, "2 × …" for two of a kind
	names             []string   // each card's, with more than one
	util, temp, power *float64   // averaged, hottest, summed
	cards             []GPU      // each card's, with more than one
	detail            []Card     // each card's fuller reading, one or more
	detailInfo        []CardInfo // and what the card is
}

const (
	Interval = 2 * time.Second
	// Keep is an hour of samples: the Activity Monitor's histories are an
	// art pixel a sample, and a wide window shows that many.
	Keep = 1800
	// Newest is what a reader that names no limit gets: ten minutes, all
	// the ring held before it held an hour.
	Newest = 300
	idle   = 10 * time.Minute
)

// Monitor holds the ring of samples and the sampling loop.
type Monitor struct {
	interval time.Duration
	idle     time.Duration
	read     func() counters
	hist     *History // the averages kept on disk; nil for none

	mu      sync.Mutex
	ring    []Sample
	prev    *counters
	info    Info
	wanted  time.Time
	running bool
	always  bool // sampling whether or not anyone reads (Start)
}

// New samples the real host. Supported says whether this platform can.
func New() *Monitor { return &Monitor{interval: Interval, idle: idle, read: readHost} }

// NewWithHistory samples the real host and keeps its history in dir.
func NewWithHistory(dir string) (*Monitor, error) {
	h, err := OpenHistory(dir)
	if err != nil {
		return New(), err
	}
	m := New()
	m.hist = h
	return m, nil
}

// Start samples from now on, read or not: the history needs every slot.
func (m *Monitor) Start() {
	m.mu.Lock()
	m.always = true
	m.wanted = time.Now()
	m.mu.Unlock()
	go m.ReadDetail(time.Now().UnixMilli(), false) // starts the loop if it is not running; its first reading takes a while
}

// Read returns what the samples cover and every sample newer than after
// (unix ms; 0 for all), and keeps the sampling going: a cold monitor takes
// its first reading now, so the first sample lands one interval later.
// The per-core and per-card detail is left out (ReadDetail).
func (m *Monitor) Read(after int64) (Info, []Sample) { return m.ReadDetail(after, false) }

// ReadDetail is Read, with each core and each card when detail is true.
func (m *Monitor) ReadDetail(after int64, detail bool) (Info, []Sample) {
	info, samples, _ := m.ReadNewest(after, detail, 0)
	return info, samples
}

// ReadNewest is ReadDetail, with at most the newest limit samples (all
// with limit 0); more says older ones newer than after were left out.
func (m *Monitor) ReadNewest(after int64, detail bool, limit int) (info Info, out []Sample, more bool) {
	m.mu.Lock()
	m.wanted = time.Now()
	start := !m.running
	m.running = true
	m.mu.Unlock()
	if start {
		c := m.read()
		m.mu.Lock()
		m.prev, m.info = &c, c.info
		m.mu.Unlock()
		go m.loop()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ring := m.ring
	if limit > 0 && len(ring) > limit {
		ring = ring[len(ring)-limit:]
		more = m.ring[len(m.ring)-limit-1].T > after
	}
	out = []Sample{}
	for _, s := range ring {
		if s.T > after {
			if !detail {
				s.Cores, s.Freq, s.Cards, s.Nets = nil, nil, nil, nil
			}
			out = append(out, s)
		}
	}
	info = m.info
	if !detail {
		info.CPUs, info.CPUKinds, info.Cards, info.Ifaces = nil, nil, nil, nil
	}
	return info, out, more
}

func (m *Monitor) loop() {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for range t.C {
		m.mu.Lock()
		if !m.always && time.Since(m.wanted) > m.idle {
			m.running, m.prev = false, nil // the next reader starts afresh
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		c := m.read()
		m.mu.Lock()
		var took *Sample
		if m.prev != nil {
			if s, ok := between(m.prev, &c, m.interval); ok {
				m.ring = append(m.ring, s)
				if len(m.ring) > Keep {
					// the slice moves along its array; the next append
					// that outgrows it copies the hour to a new one
					m.ring = m.ring[len(m.ring)-Keep:]
				}
				took = &s
			}
		}
		m.prev, m.info = &c, c.info
		info := c.info
		m.mu.Unlock()
		if took != nil && m.hist != nil {
			m.hist.Add(took, info)
		}
	}
}

// between makes the sample for the span from a to b: no sample at all when
// the span is not a plausible tick (a stalled host, a clock jump).
func between(a, b *counters, interval time.Duration) (Sample, bool) {
	dt := b.at.Sub(a.at).Seconds()
	if dt <= 0 || dt > 3*interval.Seconds() {
		return Sample{}, false
	}
	s := Sample{T: b.at.UnixMilli(), MemUsed: b.memTotal - min(b.memAvail, b.memTotal)}
	if a.cpuOK && b.cpuOK && b.cpuTotal > a.cpuTotal && b.cpuBusy >= a.cpuBusy {
		s.CPU = num(min(100, float64(b.cpuBusy-a.cpuBusy)/float64(b.cpuTotal-a.cpuTotal)*100))
	}
	if b.memTotal > 0 {
		s.Mem = num(float64(s.MemUsed) / float64(b.memTotal) * 100)
	}
	s.CPUTemp = b.cpuTemp
	s.GPU, s.GPUTemp, s.GPUPower, s.GPUs = b.gpu.util, b.gpu.temp, b.gpu.power, b.gpu.cards
	s.NetIn, s.NetOut = rates(a.net, b.net, dt)
	s.DiskRead, s.DiskWrite = rates(a.disk, b.disk, dt)
	if n := len(b.info.CPUs); n > 0 && b.perCPU != nil {
		s.Cores = make([]int, n)
		for i := range s.Cores {
			s.Cores[i] = -1
			x, okA := a.perCPU[i]
			y, okB := b.perCPU[i]
			if okA && okB && y[1] > x[1] && y[0] >= x[0] {
				s.Cores[i] = int(math.Round(min(100, float64(y[0]-x[0])/float64(y[1]-x[1])*100)))
			}
		}
	}
	if len(b.freq) > 0 {
		s.Freq = b.freq
	}
	s.Cards = b.gpu.detail
	for _, ifc := range b.info.Ifaces {
		n := NetIf{Up: b.links[ifc.Name].up, Signal: b.links[ifc.Name].signal}
		x, okA := a.net[ifc.Name]
		y, okB := b.net[ifc.Name]
		if okA && okB && y[0] >= x[0] && y[1] >= x[1] { // a counter that went back (a reset) gives no figure
			n.In, n.Out = num(float64(y[0]-x[0])/dt), num(float64(y[1]-x[1])/dt)
		}
		s.Nets = append(s.Nets, n)
	}
	return s, true
}

// rates sums each device's growth over dt seconds; a device whose counter
// went backwards (reset, or re-plugged) or that just appeared adds nothing.
func rates(a, b map[string][2]uint64, dt float64) (*float64, *float64) {
	if a == nil || b == nil {
		return nil, nil
	}
	var x, y float64
	for dev, cur := range b {
		prev, ok := a[dev]
		if !ok {
			continue
		}
		if cur[0] >= prev[0] {
			x += float64(cur[0] - prev[0])
		}
		if cur[1] >= prev[1] {
			y += float64(cur[1] - prev[1])
		}
	}
	return num(x / dt), num(y / dt)
}

func num(f float64) *float64 { return &f }

// names is a device map's names, sorted.
func names(m map[string][2]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ReadHistory gives the history of a step (Steps): its newest rows up to
// limit, after after, whether older ones are kept, and the newest sample
// of the moment for the figures beside the pictures. A monitor with no
// history gives none.
func (m *Monitor) ReadHistory(step int, after int64, limit int, detail bool) (info Info, rows []Sample, more bool, now *Sample) {
	info, recent, _ := m.ReadNewest(0, detail, 1)
	if len(recent) > 0 {
		now = &recent[0]
	}
	if m.hist == nil {
		return info, []Sample{}, false, now
	}
	rows, more = m.hist.Read(step, time.Now(), after, limit)
	if rows == nil {
		rows = []Sample{}
	}
	if !detail {
		for i := range rows {
			r := &rows[i]
			r.Cores, r.Freq, r.Cards, r.Nets = nil, nil, nil, nil
			if r.Peak != nil {
				r.Peak.Cores, r.Peak.Freq, r.Peak.Cards, r.Peak.Nets = nil, nil, nil, nil
			}
		}
	}
	return info, rows, more, now
}

// Close writes what the history's slots hold so far.
func (m *Monitor) Close() error {
	if m.hist == nil {
		return nil
	}
	return m.hist.Close()
}
