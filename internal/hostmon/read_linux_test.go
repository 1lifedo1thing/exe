//go:build linux

package hostmon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseStat(t *testing.T) {
	busy, total, ok, per := parseStat(strings.NewReader("cpu  408690183 216559 52384731 5248863620 5380860 0 944647 0 9717658 0\n" +
		"cpu0 1 2 3 4 5 6 7 8 9 10\ncpu2 10 0 10 70 10 0 0 0 0 0\nintr 1 2 3\n"))
	// guest (9717658) is inside user already: total is the first eight
	wantTotal := uint64(408690183 + 216559 + 52384731 + 5248863620 + 5380860 + 0 + 944647 + 0)
	if !ok || total != wantTotal || busy != wantTotal-5248863620-5380860 {
		t.Fatalf("busy %d total %d ok %v", busy, total, ok)
	}
	// each CPU's own line; cpu1 is offline and has none
	if len(per) != 2 || per[0] != [2]uint64{1 + 2 + 3 + 6 + 7 + 8, 36} || per[2] != [2]uint64{20, 100} {
		t.Fatalf("per CPU: %v", per)
	}
	if _, _, ok, _ := parseStat(strings.NewReader("intr 1 2 3\n")); ok {
		t.Error("read a cpu line from nothing")
	}
}

func TestReadTopology(t *testing.T) {
	write := func(root, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(root+path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a Xeon of two threads a core: cpu0 and cpu2 share core 0
	x := t.TempDir()
	for i, core := range []string{"0", "1", "0", "1"} {
		d := "/cpu/cpu" + strconv.Itoa(i)
		write(x, d+"/topology/physical_package_id", "0")
		write(x, d+"/topology/core_id", core)
		write(x, d+"/cpufreq/cpuinfo_max_freq", "4800000")
		write(x, d+"/cpufreq/scaling_cur_freq", strconv.Itoa(1000000+i*500000))
	}
	var info strings.Builder
	for i := range 4 {
		info.WriteString("processor\t: " + strconv.Itoa(i) + "\nmodel name\t: Intel(R) Xeon(R) w9-3475X CPU @ 2.20GHz\n\n")
	}
	write(x, "/cpuinfo", info.String())
	tp := readTopology(x+"/cpu", x+"/cpuinfo")
	if len(tp.cpus) != 4 || len(tp.kinds) != 1 || tp.kinds[0] != "Intel Xeon w9-3475X" ||
		tp.cpus[0].Core != 0 || tp.cpus[1].Core != 1 || tp.cpus[2].Core != 0 || tp.cpus[3].MaxMHz != 4800 {
		t.Fatalf("Xeon: %+v", tp)
	}
	if f := readFreqs(x+"/cpu", 4); len(f) != 4 || f[0] != 1000 || f[3] != 2500 {
		t.Fatalf("clocks: %v", f)
	}
	// the GB10: Arm parts, each CPU its own core, two kinds
	g := t.TempDir()
	info.Reset()
	for i := range 3 {
		d := "/cpu/cpu" + strconv.Itoa(i)
		write(g, d+"/topology/physical_package_id", "0")
		write(g, d+"/topology/core_id", strconv.Itoa(i))
		part := "0xd87"
		if i == 2 {
			part = "0xd85"
		}
		info.WriteString("processor\t: " + strconv.Itoa(i) + "\nCPU implementer\t: 0x41\nCPU part\t: " + part + "\n\n")
	}
	write(g, "/cpuinfo", info.String())
	tp = readTopology(g+"/cpu", g+"/cpuinfo")
	if len(tp.kinds) != 2 || tp.kinds[0] != "Cortex-A725" || tp.kinds[1] != "Cortex-X925" ||
		tp.cpus[2].Kind != 1 || tp.cpus[2].Core != 2 || tp.cpus[0].MaxMHz != 0 {
		t.Fatalf("GB10: %+v", tp)
	}
	if tp := readTopology(t.TempDir(), "/nonexistent"); tp.cpus != nil {
		t.Fatalf("no CPUs: %+v", tp)
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

func TestReadLinks(t *testing.T) {
	root := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(root+path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// spark: a 2.5 Gb/s port up, its Wi-Fi down; a port some driver leaves
	// "unknown" with a carrier; a port down whose speed the kernel refuses
	write("/enP7s7/operstate", "up")
	write("/enP7s7/speed", "2500")
	write("/wlP9s9/operstate", "up")
	write("/wlP9s9/phy80211/name", "phy0")
	write("/eth9/operstate", "unknown")
	write("/eth9/carrier", "1")
	write("/eth9/speed", "-1")
	write("/enp1s0/operstate", "down")
	signals := parseWireless("Inter-| sta-|   Quality        |   Discarded packets               | Missed | WE\n" +
		" face | tus | link level noise |  nwid  crypt   frag  retry   misc | beacon | 22\n" +
		"wlP9s9: 0000   55.  -61.  -256        0      0      0      0      0        0\n")
	if signals["wlP9s9"] != -61 {
		t.Fatalf("signals: %v", signals)
	}
	ifaces, links := readLinks(root, []string{"enP7s7", "enp1s0", "eth9", "wlP9s9"}, signals)
	if len(ifaces) != 4 || ifaces[0].Speed != 2500 || ifaces[0].Wireless || !links["enP7s7"].up {
		t.Fatalf("the wired port: %+v %+v", ifaces, links)
	}
	if ifaces[1].Speed != 0 || links["enp1s0"].up {
		t.Fatalf("a port down: %+v %+v", ifaces[1], links["enp1s0"])
	}
	if ifaces[2].Speed != 0 || !links["eth9"].up {
		t.Fatalf("unknown with a carrier: %+v %+v", ifaces[2], links["eth9"])
	}
	if !ifaces[3].Wireless || ifaces[3].Speed != 0 || links["wlP9s9"].signal == nil || *links["wlP9s9"].signal != -61 {
		t.Fatalf("Wi-Fi: %+v %+v", ifaces[3], links["wlP9s9"])
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
	if g.names != nil || g.cards != nil {
		t.Fatalf("one GPU has no cards of its own: %+v", g)
	}
	g = parseSMI("A, 10, 50, [N/A]\nB, 30, 61, [N/A]\n")
	if g.name != "A + B" || *g.util != 20 || *g.temp != 61 || g.power != nil {
		t.Fatalf("two GPUs: %+v", g)
	}
	if len(g.cards) != 2 || *g.cards[0].Util != 10 || *g.cards[1].Temp != 61 || g.cards[1].Power != nil ||
		len(g.names) != 2 || g.names[1] != "B" {
		t.Fatalf("two GPUs, each: %+v", g)
	}
	g = parseSMI("NVIDIA RTX 6000 Ada Generation, 0, 34, 21.5\nNVIDIA RTX 6000 Ada Generation, 80, 52, 180.25\n")
	if g.name != "2 × NVIDIA RTX 6000 Ada Generation" || *g.util != 40 || *g.power != 201.75 ||
		*g.cards[0].Util != 0 || *g.cards[1].Power != 180.25 {
		t.Fatalf("two of a kind: %+v", g)
	}
	if g := parseSMI(""); g.util != nil || g.name != "" {
		t.Fatalf("no output: %+v", g)
	}
	// the full query: the GB10 has no memory, limit or fan of its own
	g = parseSMI("NVIDIA GB10, 63, 52, 25.19, 0, [N/A], [N/A], [N/A], [N/A], 2535, 3003, P0, 0, 0\n")
	if g.name != "NVIDIA GB10" || len(g.detail) != 1 || g.cards != nil {
		t.Fatalf("GB10 detail: %+v", g)
	}
	d := g.detail[0]
	ci := g.detailInfo[0]
	if *d.Util != 63 || *d.Clock != 2535 || *ci.ClockMax != 3003 || d.MemUsed != nil || ci.MemTotal != nil ||
		d.Fan != nil || ci.Fan || ci.PowerLimit != nil || d.PState != "P0" || *d.Enc != 0 || !ci.Video || ci.Name != "NVIDIA GB10" {
		t.Fatalf("GB10 card: %+v %+v", d, ci)
	}
	g = parseSMI("NVIDIA RTX 6000 Ada Generation, 0, 50, 19.86, 0, 4734, 49140, 300.00, 30, 210, 3105, P8, 0, 0\n" +
		"NVIDIA RTX 6000 Ada Generation, 97, 71, 280.5, 40, 10018, 49140, 300.00, 55, 2505, 3105, P2, 12, 3\n")
	if len(g.detail) != 2 || *g.detail[0].MemUsed != 4734<<20 || *g.detailInfo[1].MemTotal != 49140<<20 ||
		*g.detail[1].Fan != 55 || !g.detailInfo[1].Fan || *g.detailInfo[1].PowerLimit != 300 || *g.detail[1].Dec != 3 || *g.util != 48.5 {
		t.Fatalf("two cards' detail: %+v %+v", g.detail, g)
	}
	// a driver of the short query: the cards are named, their detail empty
	g = parseSMI("NVIDIA GB10, 37, 46, 10.90\n")
	if len(g.detailInfo) != 1 || g.detailInfo[0].Name != "NVIDIA GB10" || g.detailInfo[0].Fan || *g.detail[0].Util != 37 {
		t.Fatalf("short query: %+v", g)
	}
}

func TestCPUTempSensors(t *testing.T) {
	write := func(root, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(root+path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(root+path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// the GB10: seven ACPI zones, four of them the CPU clusters
	gb10 := t.TempDir()
	for i, z := range []struct{ path, temp string }{
		{`\_TZ_.TSOC`, "60900"}, {`\_TZ_.TS0E`, "45300"}, {`\_TZ_.TS0P`, "46700"}, {`\_TZ_.TS1E`, "45700"},
		{`\_TZ_.TS1P`, "58400"}, {`\_TZ_.TGPU`, "70000"}, {`\_TZ_.TUNC`, "45800"},
	} {
		z0 := "/class/thermal/thermal_zone" + strconv.Itoa(i)
		write(gb10, z0+"/type", "acpitz")
		write(gb10, z0+"/device/path", z.path)
		write(gb10, z0+"/temp", z.temp)
	}
	write(gb10, "/class/hwmon/hwmon1/name", "nvme")
	write(gb10, "/class/hwmon/hwmon1/temp1_input", "41850")
	got := cpuTempSensors(gb10)
	if len(got) != 4 {
		t.Fatalf("GB10 sensors: %v", got)
	}
	// an AMD desktop: Tctl counts, the dies' inputs do not
	amd := t.TempDir()
	write(amd, "/class/hwmon/hwmon2/name", "k10temp")
	write(amd, "/class/hwmon/hwmon2/temp1_input", "51000")
	write(amd, "/class/hwmon/hwmon2/temp1_label", "Tctl")
	write(amd, "/class/hwmon/hwmon2/temp3_input", "43000")
	write(amd, "/class/hwmon/hwmon2/temp3_label", "Tccd1")
	write(amd, "/class/thermal/thermal_zone0/type", "acpitz")
	write(amd, "/class/thermal/thermal_zone0/temp", "30000")
	if got := cpuTempSensors(amd); len(got) != 1 || !strings.HasSuffix(got[0], "hwmon2/temp1_input") {
		t.Fatalf("AMD sensors: %v", got)
	}
	// a Raspberry Pi: a thermal zone typed for the CPU
	pi := t.TempDir()
	write(pi, "/class/thermal/thermal_zone0/type", "cpu-thermal")
	write(pi, "/class/thermal/thermal_zone0/temp", "48000")
	if got := cpuTempSensors(pi); len(got) != 1 {
		t.Fatalf("Pi sensors: %v", got)
	}
	if got := cpuTempSensors(t.TempDir()); len(got) != 0 {
		t.Fatalf("a machine with no sensors: %v", got)
	}
}
