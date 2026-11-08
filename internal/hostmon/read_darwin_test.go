//go:build darwin

package hostmon

import (
	"testing"
	"time"
)

func TestParseHardwarePorts(t *testing.T) {
	// planet, an M1 mini: its Ethernet and Wi-Fi count; the virtual
	// adapters and Thunderbolt's bridge and ports do not
	got := parseHardwarePorts(`
Hardware Port: Ethernet
Device: en0
Ethernet Address: 14:98:77:65:0d:4f

Hardware Port: Ethernet Adapter (en4)
Device: en4
Ethernet Address: 2e:4f:05:5a:8c:1a

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: N/A

Hardware Port: Wi-Fi
Device: en1
Ethernet Address: 2a:c7:2c:42:c2:45

Hardware Port: Thunderbolt 1
Device: en2
Ethernet Address: 36:f3:06:32:9c:00

Hardware Port: USB 10/100/1000 LAN
Device: en7
Ethernet Address: 00:e0:4c:68:00:01
`)
	if len(got) != 3 || got["en0"].wireless || !got["en1"].wireless || got["en7"].wireless {
		t.Fatalf("%+v", got)
	}
}

// The real machine: two reads a moment apart make a sample with every
// CPU, the memory, and the GPU of an Apple chip.
func TestReadHostDarwin(t *testing.T) {
	a := readHost()
	time.Sleep(300 * time.Millisecond)
	b := readHost()
	s, ok := between(&a, &b, time.Second)
	if !ok || s.CPU == nil || s.Mem == nil || len(s.Cores) != len(b.info.CPUs) || len(b.info.CPUKinds) == 0 {
		t.Fatalf("sample %+v info %+v", s, b.info)
	}
	if s.CPUTemp == nil || *s.CPUTemp < 10 || *s.CPUTemp > 120 {
		t.Errorf("CPU temperature: %v", s.CPUTemp)
	}
	t.Logf("cpu %.0f%% %.1f°C mem %.0f%% cores %v kinds %v gpu %q util %v temp %v nets %v disks %v",
		*s.CPU, *s.CPUTemp, *s.Mem, s.Cores, b.info.CPUKinds, b.info.GPU, *s.Cards[0].Util, *s.Cards[0].Temp, b.info.Ifaces, b.info.Disks)
}
