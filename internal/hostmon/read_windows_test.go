//go:build windows

package hostmon

import (
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// On a real PC: two reads two seconds apart make a whole sample.
func TestReadHostWindows(t *testing.T) {
	a := readHost()
	time.Sleep(2 * time.Second)
	b := readHost()
	s, ok := between(&a, &b, 2*time.Second)
	if !ok || s.CPU == nil || s.Mem == nil || len(s.Cores) != runtime.NumCPU() || len(b.info.CPUs) != runtime.NumCPU() || len(b.info.CPUKinds) == 0 {
		t.Fatalf("sample %+v info %+v", s, b.info)
	}
	cores := map[int]bool{}
	for _, c := range b.info.CPUs {
		cores[c.Core] = true
	}
	if len(cores) == 0 || len(cores) > runtime.NumCPU() {
		t.Errorf("cores: %v", b.info.CPUs)
	}
	clocked := 0
	for _, f := range s.Freq {
		if f > 0 {
			clocked++
		}
	}
	if clocked != runtime.NumCPU() {
		t.Errorf("clocks: %v", s.Freq)
	}
	if len(b.info.Ifaces) == 0 || len(s.Nets) != len(b.info.Ifaces) {
		t.Errorf("no network: %+v %+v", b.info.Ifaces, s.Nets)
	}
	if len(b.info.Disks) == 0 {
		t.Errorf("no disks")
	}
	info, _ := json.Marshal(b.info)
	sample, _ := json.Marshal(s)
	t.Logf("%d cores; info %s", len(cores), info)
	t.Logf("sample %s", sample)
}
