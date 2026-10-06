package cf

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestZoneForLongestHeldSuffix(t *testing.T) {
	zones := map[string]string{"example.org": "zone-org", "deep.example.org": "zone-deep"}
	var asked []string
	c := &Client{Token: "t", HTTPC: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		name := r.URL.Query().Get("name")
		asked = append(asked, name)
		result := "[]"
		if id, ok := zones[name]; ok {
			result = `[{"id":"` + id + `","name":"` + name + `"}]`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"success":true,"errors":[],"result":` + result + `}`))}, nil
	})}}
	for _, tc := range []struct{ host, zone, asked string }{
		{"coin.example.org", "zone-org", "coin.example.org,example.org"},
		{"a.deep.example.org", "zone-deep", "a.deep.example.org,deep.example.org"},
		{"example.org", "zone-org", "example.org"},
		{"coin.example.net", "", "coin.example.net,example.net"},
	} {
		asked = nil
		zone, err := c.ZoneFor(context.Background(), tc.host)
		if err != nil || zone != tc.zone || strings.Join(asked, ",") != tc.asked {
			t.Errorf("%s: zone %q err %v asked %v", tc.host, zone, err, asked)
		}
	}
}
