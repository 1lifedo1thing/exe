package hostmon

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// t0 starts a day (UTC), so every tier's slot starts with it.
const t0 = 19675 * 86400

func histInfo(ifaces ...string) Info {
	info := Info{CPUs: []CPU{{}, {}}, CPUKinds: []string{"Test"}, Cards: []CardInfo{{Name: "Test GPU"}}}
	for _, n := range ifaces {
		info.Ifaces = append(info.Ifaces, Iface{Name: n})
	}
	return info
}

// a sample at sec: the CPU at load, the first thread at load, the second
// offline, the card at util, each port moving in bytes a second
func histSample(sec int64, load, util, in float64, ports int) *Sample {
	s := &Sample{T: sec * 1000, CPU: num(load), Mem: num(40), MemUsed: 3 << 30, Cores: []int{int(load), -1}, Freq: []int{2000, 0},
		Cards: []Card{{Util: num(util), Temp: num(60), Power: num(123.4)}}}
	for range ports {
		s.Nets = append(s.Nets, NetIf{In: num(in), Out: num(in / 2), Up: true, Signal: num(-50)})
	}
	return s
}

func near(a *float64, want float64) bool { return a != nil && math.Abs(*a-want) < 0.6 }

func TestHistoryAveragesAndPeaks(t *testing.T) {
	h, err := OpenHistory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info := histInfo("eth0")
	// three minutes, a sample every 2 s: minute m at 10·m %, one sample of
	// each minute at 90 %
	for i := int64(0); i < 90; i++ {
		load := float64(10 * (i / 30))
		if i%30 == 5 {
			load = 90
		}
		h.Add(histSample(t0+2*i, load, load, 1000*float64(i%30), 1), info)
	}
	rows, more := h.Read(60, time.Unix(t0+179, 0), 0, 100)
	if len(rows) != 3 || more {
		t.Fatalf("%d rows, more %v", len(rows), more)
	}
	for m, r := range rows {
		want := (float64(10*m)*29 + 90) / 30
		if r.T != (t0+60*int64(m))*1000 || r.N != 30 || !near(r.CPU, want) || !near(r.Peak.CPU, 90) {
			t.Errorf("minute %d: t %d n %d cpu %v peak %v, want %.1f and 90", m, r.T, r.N, *r.CPU, *r.Peak.CPU, want)
		}
		if len(r.Cores) != 2 || math.Abs(float64(r.Cores[0])-want) > 0.6 || r.Cores[1] != -1 || r.Peak.Cores[0] != 90 {
			t.Errorf("minute %d threads: %v, peaks %v", m, r.Cores, r.Peak.Cores)
		}
		if len(r.Cards) != 1 || !near(r.Cards[0].Util, want) || !near(r.Cards[0].Power, 123.4) || !near(r.Peak.Cards[0].Util, 90) {
			t.Errorf("minute %d card: %+v", m, r.Cards)
		}
		if len(r.Nets) != 1 || !near(r.Nets[0].In, 14500) || !near(r.Peak.Nets[0].In, 29000) || !r.Nets[0].Up || !near(r.Nets[0].Signal, -50) {
			t.Errorf("minute %d port: in %v peak %v", m, *r.Nets[0].In, *r.Peak.Nets[0].In)
		}
		if r.MemUsed != 3<<30 || r.Freq[0] != 2000 {
			t.Errorf("minute %d: memory %d clock %v", m, r.MemUsed, r.Freq)
		}
	}
	// the coarser tiers hold the same samples in one slot
	for _, step := range []int{600, 3600, 86400} {
		rows, _ := h.Read(step, time.Unix(t0+179, 0), 0, 10)
		if len(rows) != 1 || rows[0].N != 90 || !near(rows[0].CPU, (10*29+20*29+90*3)/90.0) || !near(rows[0].Peak.CPU, 90) {
			t.Errorf("step %d: %+v", step, rows)
		}
	}
	// after: only the rows past it, the slot still filling among them
	if rows, _ := h.Read(60, time.Unix(t0+179, 0), (t0+60)*1000, 100); len(rows) != 1 || rows[0].T != (t0+120)*1000 {
		t.Errorf("after the second minute: %+v", rows)
	}
	h.Close()
}

// A daemon that starts again in the middle of a slot takes it up: the
// hour's row holds the samples from before and after.
func TestHistoryRestart(t *testing.T) {
	dir := t.TempDir()
	info := histInfo("eth0")
	h, _ := OpenHistory(dir)
	for i := int64(0); i < 60; i++ {
		h.Add(histSample(t0+2*i, 20, 0, 0, 1), info)
	}
	h.Close()
	h, _ = OpenHistory(dir)
	for i := int64(60); i < 90; i++ {
		h.Add(histSample(t0+2*i, 50, 0, 0, 1), info)
	}
	rows, _ := h.Read(3600, time.Unix(t0+180, 0), 0, 5)
	if len(rows) != 1 || rows[0].N != 90 || !near(rows[0].CPU, 30) || !near(rows[0].Peak.CPU, 50) {
		t.Fatalf("the hour across a restart: %+v", rows)
	}
	if rows, _ := h.Read(60, time.Unix(t0+180, 0), 0, 5); len(rows) != 3 || rows[0].N != 30 {
		t.Fatalf("the minutes across a restart: %d rows", len(rows))
	}
	h.Close()
}

// A port that comes keeps the history before it; one that goes keeps its
// own where the files hold some of it.
func TestHistoryLayoutChange(t *testing.T) {
	dir := t.TempDir()
	h, _ := OpenHistory(dir)
	for i := int64(0); i < 30; i++ {
		h.Add(histSample(t0+2*i, 20, 0, 100, 1), histInfo("eth0"))
	}
	for i := int64(30); i < 60; i++ {
		h.Add(histSample(t0+2*i, 40, 0, 300, 2), histInfo("eth0", "wlan0"))
	}
	rows, _ := h.Read(60, time.Unix(t0+119, 0), 0, 5)
	if len(rows) != 2 || len(rows[0].Nets) != 2 || !near(rows[0].Nets[0].In, 100) || rows[0].Nets[1].In != nil || !near(rows[1].Nets[1].In, 300) {
		t.Fatalf("a port that came: %+v", rows)
	}
	if !near(rows[0].CPU, 20) || !near(rows[1].CPU, 40) {
		t.Fatalf("the CPU across the change: %v %v", *rows[0].CPU, *rows[1].CPU)
	}
	for i := int64(60); i < 90; i++ {
		h.Add(histSample(t0+2*i, 60, 0, 500, 1), histInfo("wlan0"))
	}
	h.Close()
	for _, name := range []string{"minute", "day"} {
		f, _ := os.Open(filepath.Join(dir, name+".hist"))
		hdr, _, err := readHeader(f)
		f.Close()
		found := false
		for _, c := range hdr.Cols {
			found = found || c.Name == "net/eth0/in"
		}
		if err != nil || !found {
			t.Errorf("%s: eth0 is no longer kept: %v %v", name, err, hdr.Cols)
		}
	}
	h, _ = OpenHistory(dir)
	h.Add(histSample(t0+180, 60, 0, 500, 1), histInfo("wlan0"))
	rows, _ = h.Read(60, time.Unix(t0+181, 0), 0, 5)
	// the first minute has no figure for wlan0: no port at all, or one with none
	if len(rows) != 4 || (len(rows[0].Nets) > 0 && rows[0].Nets[0].In != nil) || !near(rows[1].Nets[0].In, 300) || !near(rows[2].Nets[0].In, 500) {
		t.Fatalf("laid out for wlan0 alone: %d rows, %+v", len(rows), rows)
	}
	h.Close()
}

// A while without samples is a gap; "more" says there is older history
// than was asked for; a slot older than its ring is not read.
func TestHistoryGapsAndMore(t *testing.T) {
	h, _ := OpenHistory(t.TempDir())
	info := histInfo("eth0")
	h.Add(histSample(t0, 10, 0, 0, 1), info)
	h.Add(histSample(t0+600, 20, 0, 0, 1), info)
	rows, more := h.Read(60, time.Unix(t0+601, 0), 0, 20)
	if len(rows) != 2 || more || rows[1].T-rows[0].T != 600000 {
		t.Fatalf("a gap: %d rows, more %v", len(rows), more)
	}
	if rows, more = h.Read(60, time.Unix(t0+601, 0), 0, 5); len(rows) != 1 || !more {
		t.Fatalf("five minutes of eleven: %d rows, more %v", len(rows), more)
	}
	// eight days on, the first minute is past its ring: the minute file
	// has nothing older, the day file has both days and an empty one
	late := int64(t0 + 8*86400)
	h.Add(histSample(late, 30, 0, 0, 1), info)
	if rows, more := h.Read(60, time.Unix(late+1, 0), 0, 20000); len(rows) != 1 || more {
		t.Fatalf("the minute ring after a week: %d rows, more %v", len(rows), more)
	}
	rows, _ = h.Read(86400, time.Unix(late+1, 0), 0, 30)
	if len(rows) != 2 || rows[0].N != 2 || rows[1].T != late*1000 {
		t.Fatalf("days: %+v", rows)
	}
	h.Close()
}

func TestHistoryKinds(t *testing.T) {
	b := make([]byte, 4)
	for _, c := range []struct {
		k        kind
		in, want float64
	}{
		{kPct, 42.4, 42}, {kPct, 130, 100}, {kTemp, 61.6, 62}, {kMHz, 4321, 4321}, {kDeciW, 123.44, 123.4},
		{kMiB, 3 << 30, 3 << 30}, {kRate, 1234.5, 1234.5}, {kDBm, -61, -61},
	} {
		c.k.encode(b, c.in)
		if got := c.k.decode(b); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("kind %d: %v → %v, want %v", c.k, c.in, got, c.want)
		}
		c.k.encode(b, math.NaN())
		if got := c.k.decode(b); !math.IsNaN(got) {
			t.Errorf("kind %d: unknown → %v", c.k, got)
		}
	}
}
