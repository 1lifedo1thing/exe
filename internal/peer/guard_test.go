package peer

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

func guardNotes(t *testing.T, disk, incoming string) (map[string]map[string]any, bool) {
	t.Helper()
	out, changed := GuardLocalWrite("Notes/notes.json", []byte(disk), []byte(incoming))
	var d struct {
		Notes []map[string]any `json:"notes"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("guarded doc does not parse: %v\n%s", err, out)
	}
	m := map[string]map[string]any{}
	for _, n := range d.Notes {
		m[n["id"].(string)] = n
	}
	return m, changed
}

// The 2026-10-09 loss: a window running Notes from before colours saved its
// copy back, every note at the stamp disk has and none with a colour.
func TestGuardKeepsAFieldTheWriterNeverKnew(t *testing.T) {
	disk := `{"notes":[{"id":"a","text":"one","color":"yellow","created":1,"updated":50},{"id":"b","text":"two","created":2,"updated":60}]}`
	old := `{"notes":[{"id":"a","text":"one","created":1,"updated":50},{"id":"b","text":"two","created":2,"updated":60}]}`
	m, changed := guardNotes(t, disk, old)
	if !changed || m["a"]["color"] != "yellow" {
		t.Fatalf("the colour must survive an old writer: changed=%v %v", changed, m["a"])
	}
	if _, has := m["b"]["color"]; has {
		t.Fatalf("no colour may be invented: %v", m["b"])
	}
}

// Choosing White removes the field and bumps the stamp: that is an edit.
func TestGuardLetsANewerEditRemoveAField(t *testing.T) {
	disk := `{"notes":[{"id":"a","text":"one","color":"yellow","created":1,"updated":50}]}`
	white := `{"notes":[{"id":"a","text":"one","created":1,"updated":70}]}`
	out, changed := GuardLocalWrite("Notes/notes.json", []byte(disk), []byte(white))
	if changed || string(out) != white {
		t.Fatalf("a newer edit is stored as sent: changed=%v %s", changed, out)
	}
}

func TestGuardKeepsTheNewerRecordAndTheOnesLeftOut(t *testing.T) {
	disk := `{"notes":[{"id":"a","text":"new words","created":1,"updated":90},{"id":"c","text":"made elsewhere","created":3,"updated":80}]}`
	stale := `{"notes":[{"id":"a","text":"old words","created":1,"updated":40},{"id":"d","text":"made here","created":4,"updated":85}]}`
	m, changed := guardNotes(t, disk, stale)
	if !changed || m["a"]["text"] != "new words" {
		t.Fatalf("an older record may not replace a newer one: %v", m["a"])
	}
	if m["c"] == nil || m["d"] == nil {
		t.Fatalf("a record left out is kept, a new one added: %v", m)
	}
}

func TestGuardLetsExpiredTombstonesGo(t *testing.T) {
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	recent := time.Now().Add(-time.Hour).UnixMilli()
	disk := `{"notes":[{"id":"a","text":"","created":1,"updated":` + strconv.FormatInt(old, 10) + `,"deleted":` + strconv.FormatInt(old, 10) + `},` +
		`{"id":"b","text":"","created":2,"updated":` + strconv.FormatInt(recent, 10) + `,"deleted":` + strconv.FormatInt(recent, 10) + `},` +
		`{"id":"c","text":"live","created":3,"updated":5}]}`
	sent := `{"notes":[{"id":"c","text":"live","created":3,"updated":5}]}`
	m, _ := guardNotes(t, disk, sent)
	if m["a"] != nil {
		t.Fatalf("a tombstone past the TTL is the app's GC: %v", m["a"])
	}
	if m["b"] == nil {
		t.Fatalf("a fresh tombstone left out is kept, or the note comes back elsewhere")
	}
}

// Blue Pencil fills in corrections in the background without bumping the
// draft's stamp: at an equal stamp the save's own fields win.
func TestGuardLetsAnEqualStampWriteItsFields(t *testing.T) {
	disk := `{"version":2,"drafts":[{"id":"d","text":"t","checked":{},"created":1,"updated":9}]}`
	sent := `{"version":2,"drafts":[{"id":"d","text":"t","checked":{"t":"T."},"created":1,"updated":9}]}`
	out, changed := GuardLocalWrite(draftsKey, []byte(disk), []byte(sent))
	if changed || string(out) != sent {
		t.Fatalf("an equal-stamp write with every field is stored as sent: changed=%v %s", changed, out)
	}
}

func TestGuardLeavesOtherShapesAlone(t *testing.T) {
	v1 := `[{"text":"bare array","done":false}]`
	disk := `{"version":2,"items":[{"id":"x","text":"t","created":1,"updated":2}]}`
	if out, changed := GuardLocalWrite("Todo/todos.json", []byte(disk), []byte(v1)); changed || string(out) != v1 {
		t.Fatalf("a document that does not parse is stored as sent")
	}
	if out, changed := GuardLocalWrite("Paint/page.json", []byte(disk), []byte(disk)); changed || string(out) != disk {
		t.Fatalf("a file that is not record-bearing is stored as sent")
	}
}

// What the guard writes keeps every field, numbers exactly, and text as
// typed (no < for a "<").
func TestGuardOutputKeepsEverything(t *testing.T) {
	disk := `{"notes":[{"id":"a","text":"x < y & z","color":"pink","shade":3,"created":1757000000123,"updated":1757000000456}]}`
	sent := `{"notes":[{"id":"a","text":"x < y & z","created":1757000000123,"updated":1757000000456}],"extra":true}`
	out, changed := GuardLocalWrite("Notes/notes.json", []byte(disk), []byte(sent))
	if !changed {
		t.Fatal("want a guarded write")
	}
	s := string(out)
	for _, want := range []string{`"color": "pink"`, `"shade": 3`, `1757000000123`, `1757000000456`, `"x < y & z"`, `"extra": true`} {
		if !contains(s, want) {
			t.Fatalf("missing %s in\n%s", want, s)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
