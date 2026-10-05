package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"exe/internal/config"
)

// The price module asks the daemon, which asks Coinbase once a minute for
// all the pairs together: spot for every pair, 24h stats where the pair has
// an order book, an error per pair that Coinbase does not know, and a 400
// for anything that is not a BASE-QUOTE pair.
func TestPricesEndpoint(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/spot/SOL-USD":
			fmt.Fprint(w, `{"data":{"amount":"97.16","base":"SOL","currency":"USD"}}`)
		case "/spot/PUMP-SOL":
			fmt.Fprint(w, `{"data":{"amount":"0.00003617043173982414495","base":"PUMP","currency":"SOL"}}`)
		case "/stats/SOL-USD":
			fmt.Fprint(w, `{"open":"101.3","high":"101.57","low":"95.71","last":"97.05","volume":"1294067.55"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"NotFound"}`)
		}
	}))
	defer up.Close()
	oldSpot, oldStats := coinbaseSpotURL, coinbaseStatsURL
	coinbaseSpotURL, coinbaseStatsURL = up.URL+"/spot/%s", up.URL+"/stats/%s"
	defer func() { coinbaseSpotURL, coinbaseStatsURL = oldSpot, oldStats }()

	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	get := func(q string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		s.handlePrices(rec, httptest.NewRequest("GET", "/v1/prices"+q, nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec, body
	}

	rec, body := get("?pairs=sol-usd,PUMP-SOL,FOO-USD,SOL-USD")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	prices := body["prices"].(map[string]any)
	sol := prices["SOL-USD"].(map[string]any)
	if sol["spot"] != 97.16 || sol["open"] != 101.3 || sol["low"] != 95.71 {
		t.Errorf("SOL-USD: %v", sol)
	}
	pump := prices["PUMP-SOL"].(map[string]any)
	if pump["spot"].(float64) < 0.000036 || pump["spot"].(float64) > 0.000037 || pump["open"] != nil {
		t.Errorf("PUMP-SOL: want a spot and no stats, got %v", pump)
	}
	foo := prices["FOO-USD"].(map[string]any)
	if foo["spot"] != nil || !strings.Contains(fmt.Sprint(foo["error"]), "does not quote") {
		t.Errorf("FOO-USD: want an error, got %v", foo)
	}
	if body["checked_at"] == nil {
		t.Error("no checked_at")
	}
	n := hits.Load()
	if n != 6 { // two calls per pair, the duplicate SOL-USD folded away
		t.Errorf("upstream hits: want 6, got %d", n)
	}

	// the same list inside the TTL is answered from the cache; force refetches
	if rec, _ := get("?pairs=SOL-USD,PUMP-SOL,FOO-USD"); rec.Code != http.StatusOK || hits.Load() != n {
		t.Errorf("cache miss: %d hits", hits.Load())
	}
	if rec, _ := get("?pairs=SOL-USD,PUMP-SOL,FOO-USD&force=1"); rec.Code != http.StatusOK || hits.Load() != n+6 {
		t.Errorf("force: %d hits", hits.Load())
	}

	many := make([]string, 13)
	for i := range many {
		many[i] = fmt.Sprintf("T%d-USD", i)
	}
	for _, q := range []string{"", "?pairs=", "?pairs=SOL", "?pairs=SOL-USD/../x", "?pairs=" + strings.Join(many, ",")} {
		if rec, _ := get(q); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: want 400, got %d", q, rec.Code)
		}
	}
}

// A pair Coinbase cannot quote falls back to Jupiter when both its sides
// have a known mint: one call for all of them, on the keyed host with the
// config's key or on the keyless one without, a dollar pair's open taken
// back from the 24-hour change, and a pair neither knows keeps both errors.
func TestPricesJupiterFallback(t *testing.T) {
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/spot/SOL-USD":
			fmt.Fprint(w, `{"data":{"amount":"100","base":"SOL","currency":"USD"}}`)
		case "/stats/SOL-USD":
			fmt.Fprint(w, `{"open":"95","high":"101","low":"94","volume":"1000"}`)
		case "/spot/SKR-USD":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer cb.Close()
	var jupHits atomic.Int32
	var jupStatus atomic.Int32
	var lastPath, lastKey, lastIDs atomic.Value
	jup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jupHits.Add(1)
		lastPath.Store(r.URL.Path)
		lastKey.Store(r.Header.Get("x-api-key"))
		lastIDs.Store(r.URL.Query().Get("ids"))
		if c := jupStatus.Load(); c != 0 {
			w.WriteHeader(int(c))
			return
		}
		fmt.Fprintf(w, `{"%s":{"usdPrice":101,"priceChange24h":1},"%s":{"usdPrice":0.25,"priceChange24h":25},"%s":{"usdPrice":0.02}}`,
			solanaMints["SOL"], solanaMints["MET"], solanaMints["SKR"])
	}))
	defer jup.Close()
	oldSpot, oldStats, oldJup, oldLite := coinbaseSpotURL, coinbaseStatsURL, jupiterPriceURL, jupiterLiteURL
	coinbaseSpotURL, coinbaseStatsURL = cb.URL+"/spot/%s", cb.URL+"/stats/%s"
	jupiterPriceURL, jupiterLiteURL = jup.URL+"/api/price/v3", jup.URL+"/lite/price/v3"
	defer func() {
		coinbaseSpotURL, coinbaseStatsURL, jupiterPriceURL, jupiterLiteURL = oldSpot, oldStats, oldJup, oldLite
	}()

	get := func(s *Server, q string) map[string]map[string]any {
		rec := httptest.NewRecorder()
		s.handlePrices(rec, httptest.NewRequest("GET", "/v1/prices"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		var body struct{ Prices map[string]map[string]any }
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.Prices
	}
	const pairs = "?pairs=SOL-USD,MET-USD,MET-SOL,SKR-USD,PUMP-USD,FOO-USD"

	s := New(&config.Config{Jupiter: config.JupiterConfig{APIKey: "k1"}}, nil, nil, "", t.TempDir())
	p := get(s, pairs)
	if jupHits.Load() != 1 || lastPath.Load() != "/api/price/v3" || lastKey.Load() != "k1" {
		t.Errorf("keyed call: %d hits, path %v, key %v", jupHits.Load(), lastPath.Load(), lastKey.Load())
	}
	ids := strings.Split(lastIDs.Load().(string), ",")
	if len(ids) != 4 { // MET, SKR, PUMP and SOL for the cross rate; never FOO, never twice
		t.Errorf("ids: %v", ids)
	}
	if q := p["SOL-USD"]; q["spot"] != 100.0 || q["open"] != 95.0 || q["source"] != nil {
		t.Errorf("SOL-USD should stay Coinbase's: %v", q)
	}
	if q := p["MET-USD"]; q["spot"] != 0.25 || q["open"] != 0.2 || q["source"] != "jupiter" || q["error"] != nil {
		t.Errorf("MET-USD: %v", q)
	}
	if q := p["MET-SOL"]; q["spot"] != 0.25/101 || q["open"] != nil || q["source"] != "jupiter" {
		t.Errorf("MET-SOL: %v", q)
	}
	if q := p["SKR-USD"]; q["spot"] != 0.02 || q["open"] != nil || q["source"] != "jupiter" {
		t.Errorf("SKR-USD (Coinbase down, no change from Jupiter): %v", q)
	}
	if q := p["PUMP-USD"]; q["spot"] != nil || !strings.Contains(fmt.Sprint(q["error"]), "does not quote this pair; jupiter has no price") {
		t.Errorf("PUMP-USD: %v", q)
	}
	if q := p["FOO-USD"]; fmt.Sprint(q["error"]) != "coinbase does not quote this pair" {
		t.Errorf("FOO-USD has no mint and should not reach Jupiter: %v", q)
	}

	s = New(&config.Config{}, nil, nil, "", t.TempDir())
	get(s, pairs)
	if jupHits.Load() != 2 || lastPath.Load() != "/lite/price/v3" || lastKey.Load() != "" {
		t.Errorf("keyless call: %d hits, path %v, key %v", jupHits.Load(), lastPath.Load(), lastKey.Load())
	}

	jupStatus.Store(http.StatusUnauthorized)
	p = get(s, pairs+"&force=1")
	if q := p["MET-USD"]; q["spot"] != nil || !strings.Contains(fmt.Sprint(q["error"]), "; jupiter answered 401") {
		t.Errorf("MET-USD with Jupiter refusing: %v", q)
	}

	// nothing missing, no call
	n := jupHits.Load()
	get(s, "?pairs=SOL-USD,FOO-USD")
	if jupHits.Load() != n {
		t.Error("Jupiter was asked although no pair needed it")
	}
}
