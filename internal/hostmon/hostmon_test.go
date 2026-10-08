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
		net:  map[string][2]uint64{"eth0": {2000 * n, 100 * n}},
		disk: map[string][2]uint64{"nvme0n1": {0, 512 * n}},
		gpu:  gpuReading{name: "Test GPU", util: num(12)},
		info: Info{Cores: 4, MemoryTotal: 1000, GPU: "Test GPU", Net: []string{"eth0"}, Disks: []string{"nvme0n1"}},
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
