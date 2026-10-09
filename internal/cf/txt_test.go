package cf

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeRecords answers the dns_records calls SetTXT and DeleteTXT make
// from a list of TXT record ids already at the name, and logs each call.
func fakeRecords(ids []string, calls *[]string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		line := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/client/v4/zones/z")
		if r.Method == "GET" {
			line += " " + r.URL.Query().Get("type") + " " + r.URL.Query().Get("name")
		}
		if r.Body != nil {
			var rec struct {
				Content string `json:"content"`
				TTL     int    `json:"ttl"`
			}
			b, _ := io.ReadAll(r.Body)
			if json.Unmarshal(b, &rec) == nil && rec.Content != "" {
				line += " " + rec.Content
			}
		}
		*calls = append(*calls, line)
		result := "{}"
		if r.Method == "GET" {
			var recs []string
			for _, id := range ids {
				recs = append(recs, `{"id":"`+id+`"}`)
			}
			result = "[" + strings.Join(recs, ",") + "]"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"success":true,"errors":[],"result":` + result + `}`))}, nil
	})}
}

// SetTXT leaves one quoted record at the name: made when there is none,
// the first rewritten and the rest dropped when there are several.
func TestSetTXT(t *testing.T) {
	for _, tc := range []struct {
		ids  []string
		want string
	}{
		{nil, `GET /dns_records TXT _dnslink.a.example.com|POST /dns_records "dnslink=/ipfs/bafyx"`},
		{[]string{"r1"}, `GET /dns_records TXT _dnslink.a.example.com|PUT /dns_records/r1 "dnslink=/ipfs/bafyx"`},
		{[]string{"r1", "r2", "r3"}, `GET /dns_records TXT _dnslink.a.example.com|PUT /dns_records/r1 "dnslink=/ipfs/bafyx"|DELETE /dns_records/r2|DELETE /dns_records/r3`},
	} {
		var calls []string
		c := &Client{Token: "t", ZoneID: "z", HTTPC: fakeRecords(tc.ids, &calls)}
		if err := c.SetTXT(context.Background(), "_dnslink.a.example.com", "dnslink=/ipfs/bafyx", 60); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(calls, "|"); got != tc.want {
			t.Errorf("%v:\n got %s\nwant %s", tc.ids, got, tc.want)
		}
	}
}

func TestDeleteTXT(t *testing.T) {
	var calls []string
	c := &Client{Token: "t", ZoneID: "z", HTTPC: fakeRecords([]string{"r1", "r2"}, &calls)}
	if err := c.DeleteTXT(context.Background(), "_dnslink.a.example.com"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(calls, "|"), `GET /dns_records TXT _dnslink.a.example.com|DELETE /dns_records/r1|DELETE /dns_records/r2`; got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}
