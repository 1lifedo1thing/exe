//go:build linux

package hostmon

import (
	"strings"
	"testing"
)

func TestParseStat(t *testing.T) {
	busy, total, ok := parseStat(strings.NewReader("cpu  408690183 216559 52384731 5248863620 5380860 0 944647 0 9717658 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n"))
	// guest (9717658) is inside user already: total is the first eight
	wantTotal := uint64(408690183 + 216559 + 52384731 + 5248863620 + 5380860 + 0 + 944647 + 0)
	if !ok || total != wantTotal || busy != wantTotal-5248863620-5380860 {
		t.Fatalf("busy %d total %d ok %v", busy, total, ok)
	}
	if _, _, ok := parseStat(strings.NewReader("intr 1 2 3\n")); ok {
		t.Error("read a cpu line from nothing")
	}
}

func TestParseNetDev(t *testing.T) {
	text := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9000 10 0 0 0 0 0 0 9000 10 0 0 0 0 0 0
enP7s7: 847248569841 600000 0 0 0 0 0 0 962038960753 700000 0 0 0 0 0 0
tap-vm1: 5 1 0 0 0 0 0 0 6 1 0 0 0 0 0 0
`
	got := parseNetDev(strings.NewReader(text), func(n string) bool { return n == "enP7s7" })
	if len(got) != 1 || got["enP7s7"] != [2]uint64{847248569841, 962038960753} {
		t.Fatalf("%v", got)
	}
}

func TestParseDiskstats(t *testing.T) {
	text := ` 259       0 nvme0n1 5000 10 80000 300 9000 20 160000 400 0 500 700 0 0 0 0 0 0
 259       1 nvme0n1p1 100 0 2000 3 0 0 0 0 0 4 3 0 0 0 0 0 0
   7       0 loop0 50 0 400 1 0 0 0 0 0 1 1 0 0 0 0 0 0
`
	got := parseDiskstats(strings.NewReader(text), func(n string) bool { return n == "nvme0n1" })
	if len(got) != 1 || got["nvme0n1"] != [2]uint64{80000 * 512, 160000 * 512} {
		t.Fatalf("%v", got)
	}
}

func TestParseSMI(t *testing.T) {
	g := parseSMI("NVIDIA GB10, 37, 46, 10.90\n")
	if g.name != "NVIDIA GB10" || *g.util != 37 || *g.temp != 46 || *g.power != 10.9 {
		t.Fatalf("%+v", g)
	}
	g = parseSMI("A, 10, 50, [N/A]\nB, 30, 61, [N/A]\n")
	if g.name != "A" || *g.util != 20 || *g.temp != 61 || g.power != nil {
		t.Fatalf("two GPUs: %+v", g)
	}
	if g := parseSMI(""); g.util != nil || g.name != "" {
		t.Fatalf("no output: %+v", g)
	}
}
