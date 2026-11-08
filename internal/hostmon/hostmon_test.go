package hostmon

import (
	"sync"
	"testing"
	"time"
)

// a fake host: each read advances the counters by fixed amounts
type fakeHost struct {
	mu sync.Mutex
	n  uint64
	at time.Time
}

func (h *fakeHost) read() counters {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	h.at = h.at.Add(20 * time.Millisecond)
	n := h.n
	return counters{
		at: h.at, cpuOK: true, cpuBusy: 25 * n, cpuTotal: 100 * n,
		memTotal: 1000, memAvail: 600,
		net:    map[string][2]uint64{"eth0": {2000 * n, 100 * n}},
		disk:   map[string][2]uint64{"nvme0n1": {0, 512 * n}},
		gpu:    gpuReading{name: "Test GPU", util: num(12), detail: []Card{{Util: num(12)}}, detailInfo: []CardInfo{{Name: "Test GPU"}}},
		perCPU: map[int][2]uint64{0: {10 * n, 100 * n}, 1: {90 * n, 100 * n}},
		freq:   []int{1000, 2000},
		links:  map[string]link{"eth0": {up: true}},
		info: Info{Cores: 4, MemoryTotal: 1000, GPU: "Test GPU", Net: []string{"eth0"}, Disks: []string{"nvme0n1"},
			CPUs: []CPU{{}, {}}, CPUKinds: []string{"Test CPU"}, Cards: []CardInfo{{Name: "Test GPU"}},
			Ifaces: []Iface{{Name: "eth0", Speed: 1000}}},
	}
}

func TestMonitorSamplesWhileRead(t *testing.T) {
	h := &fakeHost{at: time.Now()}
	m := &Monitor{interval: 20 * time.Millisecond, idle: 150 * time.Millisecond, read: h.read}
	info, s := m.Read(0)
	if len(s) != 0 || info.Cores != 4 || info.GPU != "Test GPU" {
		t.Fatalf("cold read: %+v %v", info, s)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(s) < 3 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		_, s = m.Read(0)
	}
	if len(s) < 3 {
		t.Fatalf("only %d samples", len(s))
	}
	x := s[len(s)-1]
	// 20 ms a tick: 2000 B → 100 kB/s in, 100 B → 5 kB/s out, 512 B → 25.6 kB/s written
	if *x.CPU != 25 || *x.Mem != 40 || x.MemUsed != 400 || *x.GPU != 12 ||
		*x.NetIn != 100000 || *x.NetOut != 5000 || *x.DiskRead != 0 || *x.DiskWrite != 25600 {
		t.Fatalf("sample: cpu %v mem %v gpu %v net %v/%v disk %v/%v", *x.CPU, *x.Mem, *x.GPU, *x.NetIn, *x.NetOut, *x.DiskRead, *x.DiskWrite)
	}
	if _, newer := m.Read(x.T); len(newer) > 1 {
		t.Errorf("after the newest sample: %d samples", len(newer))
	}
	// the Control Strip's read carries no detail; the Activity Monitor's does
	if info, _ := m.Read(0); x.Cores != nil || x.Cards != nil || x.Nets != nil || info.CPUs != nil || info.Cards != nil || info.Ifaces != nil {
		t.Errorf("detail in a plain read: %v %v %+v", x.Cores, x.Cards, info)
	}
	info, d := m.ReadDetail(0, true)
	y := d[len(d)-1]
	if len(info.CPUs) != 2 || info.Cards[0].Name != "Test GPU" || len(y.Cores) != 2 || y.Cores[0] != 10 || y.Cores[1] != 90 ||
		y.Freq[1] != 2000 || len(y.Cards) != 1 || *y.Cards[0].Util != 12 ||
		len(info.Ifaces) != 1 || len(y.Nets) != 1 || !y.Nets[0].Up || *y.Nets[0].In != 100000 || *y.Nets[0].Out != 5000 {
		t.Errorf("detail: %+v %+v", info, y)
	}
	if _, again := m.Read(0); again[len(again)-1].Cores != nil {
		t.Error("a detail read changed the ring")
	}
	// nobody reads: the loop stops, the samples stay
	time.Sleep(400 * time.Millisecond)
	m.mu.Lock()
	running, kept := m.running, len(m.ring)
	m.mu.Unlock()
	if running || kept < 3 {
		t.Fatalf("after idle: running %v, %d samples kept", running, kept)
	}
}

func TestBetween(t *testing.T) {
	at := time.Now()
	a := &counters{at: at, cpuOK: true, cpuBusy: 100, cpuTotal: 1000,
		net: map[string][2]uint64{"eth0": {1000, 1000}, "gone": {5, 5}}}
	b := &counters{at: at.Add(2 * time.Second), cpuOK: true, cpuBusy: 150, cpuTotal: 1100, memTotal: 100, memAvail: 25,
		net: map[string][2]uint64{"eth0": {3000, 500}, "new": {9e9, 9e9}}}
	s, ok := between(a, b, 2*time.Second)
	if !ok || *s.CPU != 50 || *s.Mem != 75 {
		t.Fatalf("cpu/mem: %v", s)
	}
	// eth0 in grew 2000 B in 2 s; its out counter went backwards (a reset)
	// and adds nothing; an interface that just appeared adds nothing
	if *s.NetIn != 1000 || *s.NetOut != 0 {
		t.Fatalf("net: %v %v", *s.NetIn, *s.NetOut)
	}
	if s.DiskRead != nil || s.GPU != nil {
		t.Error("figures the host did not give")
	}
	if _, ok := between(a, &counters{at: at.Add(time.Minute)}, 2*time.Second); ok {
		t.Error("a sample across a stalled minute")
	}
}

// An hour is kept; a reader takes the newest it names, and hears when
// older ones were left out.
func TestReadNewest(t *testing.T) {
	m := &Monitor{interval: time.Hour, idle: time.Hour, read: (&fakeHost{at: time.Now()}).read, running: true}
	for i := range Keep + 50 {
		m.ring = append(m.ring, Sample{T: int64(i + 1)})
		if len(m.ring) > Keep {
			m.ring = m.ring[len(m.ring)-Keep:]
		}
	}
	if len(m.ring) != Keep || m.ring[0].T != 51 {
		t.Fatalf("ring: %d, from %d", len(m.ring), m.ring[0].T)
	}
	_, s, more := m.ReadNewest(0, false, 300)
	if len(s) != 300 || s[0].T != Keep+50-299 || !more {
		t.Errorf("the newest 300: %d from %d, more %v", len(s), s[0].T, more)
	}
	_, s, more = m.ReadNewest(0, false, 0)
	if len(s) != Keep || more {
		t.Errorf("all: %d, more %v", len(s), more)
	}
	// after a sample: the newer ones, and nothing more to fetch before them
	_, s, more = m.ReadNewest(Keep+50-10, false, 300)
	if len(s) != 10 || more {
		t.Errorf("newer than one: %d, more %v", len(s), more)
	}
	_, s, more = m.ReadNewest(0, false, Keep+10)
	if len(s) != Keep || more {
		t.Errorf("a limit over the ring: %d, more %v", len(s), more)
	}
}

// A started monitor samples unread and keeps each sample's figures in its
// history, read back as the slot still filling.
func TestMonitorKeepsHistory(t *testing.T) {
	hist, err := OpenHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHost{at: time.Now()}
	m := &Monitor{interval: 20 * time.Millisecond, idle: 50 * time.Millisecond, read: h.read, hist: hist}
	m.Start()
	time.Sleep(300 * time.Millisecond) // past idle: a started monitor goes on
	_, rows, _, now := m.ReadHistory(60, 0, 10, true)
	if len(rows) == 0 || rows[len(rows)-1].N < 3 || now == nil {
		t.Fatalf("history: %d rows, now %v", len(rows), now)
	}
	r := rows[len(rows)-1]
	if !near(r.CPU, 25) || len(r.Cores) != 2 || r.Cores[1] != 90 || r.Peak == nil || !near(r.Peak.CPU, 25) || len(r.Nets) != 1 {
		t.Fatalf("the minute: %+v", r)
	}
	if _, plain, _, _ := m.ReadHistory(60, 0, 10, false); plain[0].Cores != nil || plain[0].Peak.Cores != nil {
		t.Error("detail in a plain history read")
	}
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if !running {
		t.Error("a started monitor stopped with nobody reading")
	}
	m.Close()
}
