package server

import (
	"reflect"
	"testing"

	"exe/internal/config"
)

// A PUT that names the services replaces them whole (a service can be
// removed), one that leaves them out keeps them, and neither touches the
// live configuration's map.
func TestMergeConfigServices(t *testing.T) {
	old := &config.Config{Services: map[string]string{"planet": "http://127.0.0.1:7799", "art": "http://127.0.0.1:7794"}}
	nc, err := mergeConfig(old, []byte(`{"services": {"planet": "http://127.0.0.1:7799", "easel": "http://127.0.0.1:7794"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"planet": "http://127.0.0.1:7799", "easel": "http://127.0.0.1:7794"}; !reflect.DeepEqual(nc.Services, want) {
		t.Fatalf("services %v, want %v", nc.Services, want)
	}
	if _, ok := old.Services["easel"]; ok || len(old.Services) != 2 {
		t.Fatalf("the live map changed: %v", old.Services)
	}
	kept, err := mergeConfig(old, []byte(`{"apps_dirs": ["/x"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(kept.Services, old.Services) || !reflect.DeepEqual(kept.AppsDirs, []string{"/x"}) {
		t.Fatalf("kept %v %v", kept.Services, kept.AppsDirs)
	}
	kept.Services["new"] = "x"
	if _, ok := old.Services["new"]; ok {
		t.Fatal("a kept map is still the live one")
	}
	if _, err := mergeConfig(old, []byte(`{"no_such_field": 1}`)); err == nil {
		t.Fatal("an unknown field passed")
	}
}
