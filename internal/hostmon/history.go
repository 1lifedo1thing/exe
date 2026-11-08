package hostmon

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// History keeps the samples' averages and peaks on disk, the way an RRD
// does: tiers of coarser steps for older times, each a file of rows that
// never grows — a minute a row for a week, ten minutes for ninety days, an
// hour for two years — and a day a row for good, in a file that grows a
// row a day. A row is a slot of its tier's step: how many samples it
// holds, and for each figure their average and their peak. A figure is a
// column, named for what it is ("cpu", "core/3", "card/0/util",
// "net/eth0/in"), in a type as small as its figure needs: a percent in a
// byte, a clock in two, a rate in four. A column no sample gave is
// unknown, and so is a slot no sample fell in: a while the daemon did not
// run is a gap.
//
// The slot still filling is written once a minute, and a daemon that
// starts in the middle of a slot takes up what the file holds for it, so a
// restart costs a day's row at most a minute. When the figures change —
// a card, a port — the files are written again with the new columns,
// matched by name: a column that went keeps its history in the files that
// still hold some (the day file always), and one that came is unknown
// before.
type History struct {
	mu        sync.Mutex
	dir       string
	key       string   // what the columns were laid out for (layoutKey)
	cols      []column // the figures a sample gives now
	info      Info     // the info they were read from
	tiers     []*tier
	lastMin   int64 // the minute the slots still filling were last written in
	lastT     int64 // the newest sample's time, unix ms
	loggedErr bool
}

// tierSpec is one tier: its step in seconds and its rows, 0 for a file
// that grows.
type tierSpec struct {
	name string
	step int64
	rows int
}

// Tiers are the steps History keeps, finest first.
var tiers = []tierSpec{
	{"minute", 60, 7 * 24 * 60},
	{"tenmin", 600, 90 * 24 * 6},
	{"hour", 3600, 2 * 365 * 24},
	{"day", 86400, 0},
}

// Steps lists the history's steps in seconds, finest first.
func Steps() []int {
	out := make([]int, len(tiers))
	for i, t := range tiers {
		out[i] = int(t.step)
	}
	return out
}

// kind is how a column's figure is kept.
type kind uint8

const (
	kPct   kind = iota // a percent, 0–100, in a byte
	kTemp              // °C, 0–254, in a byte
	kMHz               // MHz in two bytes
	kMiB               // bytes, kept in MiB, in four
	kDeciW             // watts, in tenths, in two
	kRate              // bytes a second, a float32
	kDBm               // dBm, signed, in a byte
)

func (k kind) size() int {
	switch k {
	case kPct, kTemp, kDBm:
		return 1
	case kMHz, kDeciW:
		return 2
	}
	return 4
}

type column struct {
	Name string `json:"n"`
	Kind kind   `json:"k"`
}

// columns are the figures a machine of this info gives, in a fixed order.
func columns(info Info) []column {
	c := []column{{"cpu", kPct}, {"cpu_temp", kTemp}, {"mem", kPct}, {"mem_used", kMiB}, {"gpu", kPct}, {"gpu_temp", kTemp},
		{"gpu_power", kDeciW}, {"net_in", kRate}, {"net_out", kRate}, {"disk_read", kRate}, {"disk_write", kRate}}
	for i := range info.CPUs {
		c = append(c, column{"core/" + strconv.Itoa(i), kPct}, column{"freq/" + strconv.Itoa(i), kMHz})
	}
	for k := range info.Cards {
		p := "card/" + strconv.Itoa(k) + "/"
		c = append(c, column{p + "util", kPct}, column{p + "mem_util", kPct}, column{p + "mem_used", kMiB}, column{p + "temp", kTemp},
			column{p + "power", kDeciW}, column{p + "fan", kPct}, column{p + "clock", kMHz}, column{p + "enc", kPct}, column{p + "dec", kPct})
	}
	for _, f := range info.Ifaces {
		p := "net/" + f.Name + "/"
		c = append(c, column{p + "in", kRate}, column{p + "out", kRate}, column{p + "up", kPct}, column{p + "signal", kDBm})
	}
	return c
}

func layoutKey(info Info) string {
	names := make([]string, len(info.Ifaces))
	for i, f := range info.Ifaces {
		names[i] = f.Name
	}
	return fmt.Sprintf("%d/%d/%s", len(info.CPUs), len(info.Cards), strings.Join(names, "\x00"))
}

// field reaches one column's figure in a sample: the place of a thread, a
// card or a port in it, by the info it was read with.
type field func(s *Sample, set bool, v float64) (float64, bool)

func ptrField(p func(s *Sample) **float64) field {
	return func(s *Sample, set bool, v float64) (float64, bool) {
		f := p(s)
		if set {
			*f = num(v)
		}
		if *f == nil {
			return 0, false
		}
		return **f, true
	}
}

// fieldFor binds a column's name to its place in a sample of this info;
// nil where the info has no such thing (a card that went).
func fieldFor(name string, info Info) field {
	switch name {
	case "cpu":
		return ptrField(func(s *Sample) **float64 { return &s.CPU })
	case "cpu_temp":
		return ptrField(func(s *Sample) **float64 { return &s.CPUTemp })
	case "mem":
		return ptrField(func(s *Sample) **float64 { return &s.Mem })
	case "gpu":
		return ptrField(func(s *Sample) **float64 { return &s.GPU })
	case "gpu_temp":
		return ptrField(func(s *Sample) **float64 { return &s.GPUTemp })
	case "gpu_power":
		return ptrField(func(s *Sample) **float64 { return &s.GPUPower })
	case "net_in":
		return ptrField(func(s *Sample) **float64 { return &s.NetIn })
	case "net_out":
		return ptrField(func(s *Sample) **float64 { return &s.NetOut })
	case "disk_read":
		return ptrField(func(s *Sample) **float64 { return &s.DiskRead })
	case "disk_write":
		return ptrField(func(s *Sample) **float64 { return &s.DiskWrite })
	case "mem_used":
		return func(s *Sample, set bool, v float64) (float64, bool) {
			if set {
				s.MemUsed = uint64(v)
			}
			return float64(s.MemUsed), s.MemUsed > 0
		}
	}
	part := strings.Split(name, "/")
	switch {
	case (part[0] == "core" || part[0] == "freq") && len(part) == 2:
		i, err := strconv.Atoi(part[1])
		if err != nil || i >= len(info.CPUs) {
			return nil
		}
		n := len(info.CPUs)
		return func(s *Sample, set bool, v float64) (float64, bool) {
			p := &s.Cores
			if part[0] == "freq" {
				p = &s.Freq
			}
			if set {
				if len(*p) < n {
					*p = append(*p, make([]int, n-len(*p))...)
					if part[0] == "core" {
						for j := range *p {
							(*p)[j] = -2 // unknown, until set
						}
					}
				}
				(*p)[i] = int(math.Round(v))
			}
			if i >= len(*p) || (*p)[i] < 0 || (part[0] == "freq" && (*p)[i] == 0) {
				return 0, false
			}
			return float64((*p)[i]), true
		}
	case part[0] == "card" && len(part) == 3:
		k, err := strconv.Atoi(part[1])
		if err != nil || k >= len(info.Cards) {
			return nil
		}
		n := len(info.Cards)
		var pick func(c *Card) **float64
		switch part[2] {
		case "util":
			pick = func(c *Card) **float64 { return &c.Util }
		case "mem_util":
			pick = func(c *Card) **float64 { return &c.MemUtil }
		case "mem_used":
			pick = func(c *Card) **float64 { return &c.MemUsed }
		case "temp":
			pick = func(c *Card) **float64 { return &c.Temp }
		case "power":
			pick = func(c *Card) **float64 { return &c.Power }
		case "fan":
			pick = func(c *Card) **float64 { return &c.Fan }
		case "clock":
			pick = func(c *Card) **float64 { return &c.Clock }
		case "enc":
			pick = func(c *Card) **float64 { return &c.Enc }
		case "dec":
			pick = func(c *Card) **float64 { return &c.Dec }
		default:
			return nil
		}
		return func(s *Sample, set bool, v float64) (float64, bool) {
			if set && len(s.Cards) < n {
				s.Cards = append(s.Cards, make([]Card, n-len(s.Cards))...)
			}
			if k >= len(s.Cards) {
				return 0, false
			}
			f := pick(&s.Cards[k])
			if set {
				*f = num(v)
			}
			if *f == nil {
				return 0, false
			}
			return **f, true
		}
	case part[0] == "net" && len(part) >= 3:
		ifName, what := strings.Join(part[1:len(part)-1], "/"), part[len(part)-1]
		i := slices.IndexFunc(info.Ifaces, func(f Iface) bool { return f.Name == ifName })
		if i < 0 {
			return nil
		}
		n := len(info.Ifaces)
		return func(s *Sample, set bool, v float64) (float64, bool) {
			if set && len(s.Nets) < n {
				s.Nets = append(s.Nets, make([]NetIf, n-len(s.Nets))...)
			}
			if i >= len(s.Nets) {
				return 0, false
			}
			x := &s.Nets[i]
			switch what {
			case "in", "out", "signal":
				f := &x.In
				if what == "out" {
					f = &x.Out
				} else if what == "signal" {
					f = &x.Signal
				}
				if set {
					*f = num(v)
				}
				if *f == nil {
					return 0, false
				}
				return **f, true
			case "up":
				if set {
					x.Up = v >= 50 // up the most of the slot
				}
				if x.Up {
					return 100, true
				}
				return 0, true
			}
			return 0, false
		}
	}
	return nil
}

// encode puts a figure in its column's bytes; NaN is unknown.
func (k kind) encode(b []byte, v float64) {
	le := binary.LittleEndian
	unknown := math.IsNaN(v)
	clamp := func(lo, hi float64) float64 { return math.Max(lo, math.Min(hi, math.Round(v))) }
	switch k {
	case kPct, kTemp:
		if unknown {
			b[0] = 0xFF
		} else if k == kPct {
			b[0] = byte(clamp(0, 100))
		} else {
			b[0] = byte(clamp(0, 254))
		}
	case kDBm:
		if unknown {
			b[0] = 0x80
		} else {
			b[0] = byte(int8(clamp(-127, 127)))
		}
	case kMHz:
		if unknown {
			le.PutUint16(b, 0xFFFF)
		} else {
			le.PutUint16(b, uint16(clamp(0, 65534)))
		}
	case kDeciW:
		if unknown {
			le.PutUint16(b, 0xFFFF)
		} else {
			le.PutUint16(b, uint16(math.Max(0, math.Min(65534, math.Round(v*10)))))
		}
	case kMiB:
		if unknown {
			le.PutUint32(b, 0xFFFFFFFF)
		} else {
			le.PutUint32(b, uint32(math.Max(0, math.Min(0xFFFFFFFE, math.Round(v/(1<<20))))))
		}
	case kRate:
		if unknown {
			le.PutUint32(b, math.Float32bits(float32(math.NaN())))
		} else {
			le.PutUint32(b, math.Float32bits(float32(v)))
		}
	}
}

func (k kind) decode(b []byte) float64 {
	le := binary.LittleEndian
	switch k {
	case kPct, kTemp:
		if b[0] == 0xFF {
			return math.NaN()
		}
		return float64(b[0])
	case kDBm:
		if b[0] == 0x80 {
			return math.NaN()
		}
		return float64(int8(b[0]))
	case kMHz, kDeciW:
		v := le.Uint16(b)
		if v == 0xFFFF {
			return math.NaN()
		}
		if k == kDeciW {
			return float64(v) / 10
		}
		return float64(v)
	case kMiB:
		v := le.Uint32(b)
		if v == 0xFFFFFFFF {
			return math.NaN()
		}
		return float64(v) * (1 << 20)
	case kRate:
		return float64(math.Float32frombits(le.Uint32(b)))
	}
	return math.NaN()
}

// ---- a tier's file -----------------------------------------------------------

const (
	histMagic = "EXEHIST1"
	rowHead   = 8 // the slot (uint32), the samples (uint16), two bytes spare
)

type histHeader struct {
	Step  int64    `json:"step"`
	Rows  int      `json:"rows"`            // 0: the file grows
	First uint32   `json:"first,omitempty"` // a growing file's first slot
	Cols  []column `json:"cols"`
}

type tier struct {
	spec  tierSpec
	path  string
	f     *os.File
	hdr   histHeader
	off   int64 // where the rows start
	size  int   // a row's bytes
	pos   []int // each column's offset in a row
	rows  int   // a growing file's rows
	from  []int // each column's place in History.cols, -1 for one gone
	acc   accum
	first uint32 // the oldest slot with data, 0 for none yet
}

// accum is the slot still filling: its samples, and each column's sum,
// count and peak.
type accum struct {
	slot uint32
	n    int
	sum  []float64
	cnt  []int
	max  []float64
}

func (t *tier) layout() {
	t.pos = make([]int, len(t.hdr.Cols))
	at := rowHead
	for i, c := range t.hdr.Cols {
		t.pos[i] = at
		at += 2 * c.Kind.size()
	}
	t.size = at
}

func openTier(dir string, spec tierSpec) (*tier, error) {
	t := &tier{spec: spec, path: filepath.Join(dir, spec.name+".hist")}
	f, err := os.OpenFile(t.path, os.O_RDWR, 0o644)
	if err != nil {
		return t, err // not there yet: laid out at the first sample
	}
	hdr, off, err := readHeader(f)
	if err != nil || hdr.Step != spec.step || hdr.Rows != spec.rows {
		f.Close()
		return t, fmt.Errorf("%s: not a history of this tier: %v", t.path, err)
	}
	t.f, t.hdr, t.off = f, hdr, off
	t.layout()
	if fi, err := f.Stat(); err == nil && spec.rows == 0 {
		t.rows = int((fi.Size() - off) / int64(t.size))
	}
	return t, nil
}

func readHeader(f *os.File) (histHeader, int64, error) {
	var h histHeader
	head := make([]byte, 12)
	if _, err := f.ReadAt(head, 0); err != nil {
		return h, 0, err
	}
	if string(head[:8]) != histMagic {
		return h, 0, errors.New("no history header")
	}
	n := binary.LittleEndian.Uint32(head[8:])
	if n > 16<<20 {
		return h, 0, errors.New("a header too long")
	}
	js := make([]byte, n)
	if _, err := f.ReadAt(js, 12); err != nil {
		return h, 0, err
	}
	if err := json.Unmarshal(js, &h); err != nil {
		return h, 0, err
	}
	return h, 12 + int64(n), nil
}

// create writes a new file for these columns, beside the one it replaces,
// and moves it into place.
func (t *tier) create(cols []column, first uint32, rows [][]byte, rowSlots []uint32) error {
	hdr := histHeader{Step: t.spec.step, Rows: t.spec.rows, Cols: cols}
	if t.spec.rows == 0 {
		hdr.First = first
	}
	js, _ := json.Marshal(hdr)
	var b bytes.Buffer
	b.WriteString(histMagic)
	binary.Write(&b, binary.LittleEndian, uint32(len(js)))
	b.Write(js)
	nt := &tier{spec: t.spec, path: t.path, hdr: hdr, off: int64(b.Len())}
	nt.layout()
	tmp := t.path + ".new"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b.Bytes()); err == nil {
		if t.spec.rows > 0 {
			err = f.Truncate(nt.off + int64(t.spec.rows*nt.size))
		}
		for i, r := range rows {
			if err == nil {
				err = nt.writeAt(f, rowSlots[i], r)
			}
		}
		if err == nil {
			err = f.Sync()
		}
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	f.Close()
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
	if err := os.Rename(tmp, t.path); err != nil {
		os.Remove(tmp)
		return err
	}
	if t.f, err = os.OpenFile(t.path, os.O_RDWR, 0o644); err != nil {
		return err
	}
	t.hdr, t.off, t.size, t.pos = nt.hdr, nt.off, nt.size, nt.pos
	t.rows = 0
	if t.spec.rows == 0 {
		if fi, err := t.f.Stat(); err == nil {
			t.rows = int((fi.Size() - t.off) / int64(t.size))
		}
	}
	return nil
}

// index is where a slot's row is: its place in the ring, or in a growing
// file counted from its first slot; -1 for a slot before that.
func (t *tier) index(slot uint32) int64 {
	if t.spec.rows > 0 {
		return int64(slot) % int64(t.spec.rows)
	}
	if slot < t.hdr.First {
		return -1
	}
	return int64(slot - t.hdr.First)
}

// writeAt puts a row in a file laid out as t is; a growing file grows to
// it, its days without samples left empty.
func (t *tier) writeAt(f *os.File, slot uint32, row []byte) error {
	i := t.index(slot)
	if i < 0 {
		return nil
	}
	_, err := f.WriteAt(row, t.off+i*int64(t.size))
	return err
}

// readRow gives a slot's row, or nil when the file holds none for it.
func (t *tier) readRow(slot uint32) []byte {
	if t.f == nil {
		return nil
	}
	i := t.index(slot)
	if i < 0 || (t.spec.rows == 0 && i >= int64(t.rows)) {
		return nil
	}
	row := make([]byte, t.size)
	if _, err := t.f.ReadAt(row, t.off+i*int64(t.size)); err != nil {
		return nil
	}
	if binary.LittleEndian.Uint32(row) != slot || binary.LittleEndian.Uint16(row[4:]) == 0 {
		return nil
	}
	return row
}

// rowOf makes a slot's row from its accumulator.
func (t *tier) rowOf(a *accum) []byte {
	row := make([]byte, t.size)
	binary.LittleEndian.PutUint32(row, a.slot)
	binary.LittleEndian.PutUint16(row[4:], uint16(min(a.n, 0xFFFF)))
	for i, c := range t.hdr.Cols {
		avg, peak := math.NaN(), math.NaN()
		if a.cnt[i] > 0 {
			avg, peak = a.sum[i]/float64(a.cnt[i]), a.max[i]
		}
		w := c.Kind.size()
		c.Kind.encode(row[t.pos[i]:], avg)
		c.Kind.encode(row[t.pos[i]+w:], peak)
	}
	return row
}

// seed starts a slot's accumulator from the row the file holds for it, if
// any: a daemon that started in the middle of the slot.
func (t *tier) seed(slot uint32) accum {
	n := len(t.hdr.Cols)
	a := accum{slot: slot, sum: make([]float64, n), cnt: make([]int, n), max: make([]float64, n)}
	row := t.readRow(slot)
	if row == nil {
		return a
	}
	a.n = int(binary.LittleEndian.Uint16(row[4:]))
	for i, c := range t.hdr.Cols {
		w := c.Kind.size()
		avg, peak := c.Kind.decode(row[t.pos[i]:]), c.Kind.decode(row[t.pos[i]+w:])
		if !math.IsNaN(avg) {
			a.sum[i], a.cnt[i], a.max[i] = avg*float64(a.n), a.n, peak
		}
	}
	return a
}

func (t *tier) write(a *accum) error {
	if t.f == nil || a.n == 0 {
		return nil
	}
	if err := t.writeAt(t.f, a.slot, t.rowOf(a)); err != nil {
		return err
	}
	if i := t.index(a.slot); t.spec.rows == 0 && int(i) >= t.rows {
		t.rows = int(i) + 1
	}
	if t.first == 0 || a.slot < t.first {
		t.first = a.slot
	}
	return nil
}

// oldest finds the tier's oldest slot with data, reading each row's slot
// once: what "more" is answered from.
func (t *tier) oldest(newest uint32) uint32 {
	if t.f == nil {
		return 0
	}
	n := t.spec.rows
	if n == 0 {
		n = t.rows
	}
	var best uint32
	head := make([]byte, rowHead)
	for i := range n {
		if _, err := t.f.ReadAt(head, t.off+int64(i)*int64(t.size)); err != nil {
			break
		}
		s := binary.LittleEndian.Uint32(head)
		if s == 0 || binary.LittleEndian.Uint16(head[4:]) == 0 || s > newest {
			continue
		}
		if t.spec.rows > 0 && int64(newest)-int64(s) >= int64(t.spec.rows) {
			continue // overwritten in all but name: older than the ring
		}
		if best == 0 || s < best {
			best = s
		}
	}
	return best
}

// ---- History -----------------------------------------------------------------

// OpenHistory opens, or starts, the history kept in dir.
func OpenHistory(dir string) (*History, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	h := &History{dir: dir}
	for _, spec := range tiers {
		t, err := openTier(dir, spec)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("activity history: %v — starting it again", err)
			os.Remove(t.path)
		}
		h.tiers = append(h.tiers, t)
	}
	return h, nil
}

// Close closes the files; the slots still filling are written first.
func (h *History) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, t := range h.tiers {
		t.write(&t.acc)
		if t.f != nil {
			t.f.Close()
			t.f = nil
		}
	}
	return nil
}

// Add takes a sample with its detail into each tier.
func (h *History) Add(s *Sample, info Info) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.T <= h.lastT {
		return // the clock went back: what it says again is not taken twice
	}
	h.lastT = s.T
	if k := layoutKey(info); k != h.key || h.cols == nil {
		h.relayout(info, k)
	}
	vals := make([]float64, len(h.cols))
	for i, c := range h.cols {
		vals[i] = math.NaN()
		if f := fieldFor(c.Name, h.info); f != nil {
			if v, ok := f(s, false, 0); ok {
				vals[i] = v
			}
		}
	}
	sec := s.T / 1000
	var err error
	for _, t := range h.tiers {
		slot := uint32(sec / t.spec.step)
		if t.acc.slot != slot {
			if e := t.write(&t.acc); e != nil {
				err = e
			}
			t.acc = t.seed(slot)
		}
		a := &t.acc
		a.n++
		for i, j := range t.from {
			if j < 0 || math.IsNaN(vals[j]) {
				continue
			}
			v := vals[j]
			if a.cnt[i] == 0 || v > a.max[i] {
				a.max[i] = v
			}
			a.sum[i] += v
			a.cnt[i]++
		}
	}
	// the slots still filling, once a minute
	if m := sec / 60; m != h.lastMin {
		h.lastMin = m
		for _, t := range h.tiers[1:] {
			if e := t.write(&t.acc); e != nil {
				err = e
			}
		}
	}
	if err != nil && !h.loggedErr {
		h.loggedErr = true
		log.Printf("activity history: %v", err)
	}
}

// relayout lays the tiers out for the figures this info gives: a file
// whose columns differ is written again, its rows moved column by column.
func (h *History) relayout(info Info, key string) {
	h.key, h.info, h.cols = key, info, columns(info)
	now := uint32(h.lastT / 1000) // the sample's time, not the clock's
	for _, t := range h.tiers {
		t.write(&t.acc) // what the slot holds so far, in the old columns
		want := h.cols
		if t.f != nil && !slices.Equal(t.hdr.Cols, h.cols) {
			want = h.merged(t, now)
		}
		if t.f == nil || !slices.Equal(t.hdr.Cols, want) {
			if err := h.rewrite(t, want, now); err != nil {
				log.Printf("activity history: %s: %v", t.path, err)
			}
		}
		t.from = make([]int, len(t.hdr.Cols))
		for i, c := range t.hdr.Cols {
			t.from[i] = slices.IndexFunc(h.cols, func(x column) bool { return x == c })
		}
		t.acc = accum{} // taken up again from the file at the next sample
		t.first = t.oldest(now / uint32(t.spec.step))
	}
}

// merged is the columns a tier keeps: this machine's, then those of its
// own that are gone from it but still hold history here — in the day
// file, all of them.
func (h *History) merged(t *tier, now uint32) []column {
	out := slices.Clone(h.cols)
	for i, c := range t.hdr.Cols {
		if slices.Contains(out, c) {
			continue
		}
		if t.spec.rows > 0 && !t.hasData(i, now/uint32(t.spec.step)) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (t *tier) hasData(col int, newest uint32) bool {
	n := t.spec.rows
	c := t.hdr.Cols[col]
	buf := make([]byte, t.size)
	for i := range n {
		if _, err := t.f.ReadAt(buf, t.off+int64(i)*int64(t.size)); err != nil {
			return false
		}
		s := binary.LittleEndian.Uint32(buf)
		if s == 0 || int64(newest)-int64(s) >= int64(n) {
			continue
		}
		if !math.IsNaN(c.Kind.decode(buf[t.pos[col]:])) {
			return true
		}
	}
	return false
}

// rewrite writes a tier's file again with these columns, each row's
// figures moved to the column of their name.
func (h *History) rewrite(t *tier, cols []column, now uint32) error {
	var rows [][]byte
	var slots []uint32
	first := uint32(0)
	if t.f != nil {
		n := t.spec.rows
		if n == 0 {
			n, first = t.rows, t.hdr.First
		}
		nt := &tier{spec: t.spec, hdr: histHeader{Cols: cols}}
		nt.layout()
		buf := make([]byte, t.size)
		for i := range n {
			if _, err := t.f.ReadAt(buf, t.off+int64(i)*int64(t.size)); err != nil && err != io.EOF {
				return err
			}
			slot := binary.LittleEndian.Uint32(buf)
			if slot == 0 {
				continue
			}
			row := make([]byte, nt.size)
			copy(row, buf[:rowHead])
			for j, c := range cols {
				w := c.Kind.size()
				avg, peak := math.NaN(), math.NaN()
				if k := slices.Index(t.hdr.Cols, c); k >= 0 {
					avg, peak = c.Kind.decode(buf[t.pos[k]:]), c.Kind.decode(buf[t.pos[k]+w:])
				}
				c.Kind.encode(row[nt.pos[j]:], avg)
				c.Kind.encode(row[nt.pos[j]+w:], peak)
			}
			rows, slots = append(rows, row), append(slots, slot)
		}
	}
	if first == 0 {
		first = now / uint32(t.spec.step)
		for _, s := range slots {
			first = min(first, s)
		}
	}
	return t.create(cols, first, rows, slots)
}

// Read gives a tier's newest rows up to limit, the oldest first, each a
// sample of the slot's averages with its peaks in Peak, the slot still
// filling the newest of them; and whether the tier holds older ones. A row
// whose time is not after after is left out. The figures are laid out
// for info, the machine as it is now.
func (h *History) Read(step int, now time.Time, after int64, limit int) (rows []Sample, more bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var t *tier
	for _, x := range h.tiers {
		if x.spec.step == int64(step) {
			t = x
		}
	}
	if t == nil || limit <= 0 {
		return nil, false
	}
	fields := make([]field, len(t.hdr.Cols))
	for i, c := range t.hdr.Cols {
		fields[i] = fieldFor(c.Name, h.info)
	}
	newest := uint32(now.Unix() / t.spec.step)
	oldestShown := uint32(0)
	if int64(newest) >= int64(limit) {
		oldestShown = newest - uint32(limit) + 1
	}
	for slot := newest; ; slot-- {
		at := int64(slot) * t.spec.step * 1000
		if at <= after || (t.spec.rows > 0 && int64(newest)-int64(slot) >= int64(t.spec.rows)) {
			break
		}
		var row []byte
		if slot == t.acc.slot && t.acc.n > 0 {
			row = t.rowOf(&t.acc)
		} else {
			row = t.readRow(slot)
		}
		if row != nil {
			rows = append(rows, t.sampleOf(row, at, fields))
		}
		if slot == oldestShown || slot == 0 {
			break
		}
	}
	slices.Reverse(rows)
	first := t.first
	if t.spec.rows > 0 && int64(newest)-int64(first) >= int64(t.spec.rows) {
		first = newest - uint32(t.spec.rows) + 1 // the ring has gone round since
	}
	more = first != 0 && first < oldestShown && int64(oldestShown)*t.spec.step*1000 > after
	return rows, more
}

func (t *tier) sampleOf(row []byte, at int64, fields []field) Sample {
	s := Sample{T: at, N: int(binary.LittleEndian.Uint16(row[4:]))}
	p := &Sample{T: at}
	for i, c := range t.hdr.Cols {
		f := fields[i]
		if f == nil {
			continue // a figure of something this machine has no more
		}
		w := c.Kind.size()
		if v := c.Kind.decode(row[t.pos[i]:]); !math.IsNaN(v) {
			f(&s, true, v)
		}
		if v := c.Kind.decode(row[t.pos[i]+w:]); !math.IsNaN(v) {
			f(p, true, v)
		}
	}
	for _, x := range []*Sample{&s, p} {
		for j, v := range x.Cores {
			if v == -2 {
				x.Cores[j] = -1 // a thread the slot has no figure for reads as offline
			}
		}
	}
	s.Peak = p
	return s
}
