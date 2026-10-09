package peer

import (
	"bytes"
	"encoding/json"
	"path"
	"time"
)

// GuardLocalWrite judges an app's own save of a record-bearing document
// (the files Mergeable names) against the copy on disk, record by record,
// so a window running older code — asleep on another device with what it
// read hours ago, or written before a field existed — cannot take back
// what newer windows wrote. An app saves its whole document, and the apps'
// contract bumps a record's updated stamp on every change, so:
//
//   - a record older than disk's is stale: disk's is kept;
//   - a record the save leaves out is kept (deletions come as tombstones;
//     a tombstone past the 30-day TTL is the app's GC and goes);
//   - at an equal stamp the save's fields win (a background write such as
//     Blue Pencil's corrections keeps its stamp), but a field the save does
//     not carry at all is taken from disk: the writer never knew of it,
//     since removing one deliberately is a change and bumps the stamp.
//
// The last rule is what lost Notes' colours on 2026-10-09: an iPad still
// running the Notes from before colours wrote its copy back, every note at
// its old stamp and none with a colour, and the newer windows took it.
//
// It works on generic JSON objects, never typed records, so no field it does
// not know is ever dropped. changed is false, and incoming is returned as
// sent, when the save takes nothing from disk or either side does not
// parse.
func GuardLocalWrite(key string, disk, incoming []byte) (doc []byte, changed bool) {
	field := recordField(key)
	if field == "" || !Mergeable(key) {
		return incoming, false
	}
	_, onDisk, ok := parseRecordDoc(disk, field)
	if !ok {
		return incoming, false
	}
	top, sent, ok := parseRecordDoc(incoming, field)
	if !ok {
		return incoming, false
	}
	byID := make(map[string]map[string]any, len(onDisk))
	for _, d := range onDisk {
		byID[d["id"].(string)] = d
	}
	now := time.Now().UnixMilli()
	out := make([]any, 0, len(sent)+len(onDisk))
	seen := make(map[string]bool, len(sent))
	for _, rec := range sent {
		id := rec["id"].(string)
		seen[id] = true
		d, ok := byID[id]
		if !ok {
			out = append(out, rec)
			continue
		}
		su, du := stamp(rec, "updated"), stamp(d, "updated")
		switch {
		case su < du:
			out = append(out, d)
			changed = true
		case su == du:
			for k, v := range d {
				if _, has := rec[k]; !has {
					rec[k] = v
					changed = true
				}
			}
			out = append(out, rec)
		default:
			out = append(out, rec)
		}
	}
	for _, d := range onDisk {
		id := d["id"].(string)
		if seen[id] {
			continue
		}
		if del := stamp(d, "deleted"); del != 0 && now-del > tombstoneTTL.Milliseconds() {
			continue
		}
		out = append(out, d)
		changed = true
	}
	if !changed {
		return incoming, false
	}
	top[field] = out
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if enc.Encode(top) != nil {
		return incoming, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// recordField names the array a mergeable document keeps its records in.
func recordField(key string) string {
	switch path.Base(key) {
	case "notes.json":
		return "notes"
	case "todos.json":
		return "items"
	}
	switch key {
	case clocksKey, placesKey:
		return "items"
	case draftsKey:
		return "drafts"
	}
	return ""
}

// parseRecordDoc reads a document as an object whose field is an array of
// objects, each with a string id. A bare array (Todo's v1) or any other
// shape does not parse.
func parseRecordDoc(b []byte, field string) (map[string]any, []map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top map[string]any
	if dec.Decode(&top) != nil || top == nil {
		return nil, nil, false
	}
	arr, ok := top[field].([]any)
	if !ok {
		return nil, nil, false
	}
	recs := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, false
		}
		if id, ok := m["id"].(string); !ok || id == "" {
			return nil, nil, false
		}
		recs = append(recs, m)
	}
	return top, recs, true
}

// stamp reads an epoch-ms field; absent or unreadable is 0.
func stamp(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
		if f, err := v.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(v)
	}
	return 0
}
