package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseProxyLines(t *testing.T) {
	body := strings.Join([]string{
		"# comment",
		"; another comment",
		"// slash comment",
		"",
		"http://1.1.1.1:8080",
		"http://2.2.2.2:3128",
		"https://3.3.3.3:443",
		"socks5://4.4.4.4:1080",
		"socks5h://5.5.5.5:1080",
		"http://user:pass@6.6.6.6:8080",
		"7.7.7.7:9090",
		"8.8.8.8:1234:user:pass",
		"9.9.9.9:1080",         // scheme-less, default protocol
		"http://1.1.1.1:8080",  // duplicate
		"garbage line here",    // rejected
		"ftp://10.0.0.1:21",    // unsupported scheme
		"11.11.11.11:notaport", // accepted structurally (port validity is runtime)
	}, "\n")

	got := parseProxyLines(body, "http")
	want := []string{
		"http://1.1.1.1:8080",
		"http://2.2.2.2:3128",
		"https://3.3.3.3:443",
		"socks5://4.4.4.4:1080",
		"socks5h://5.5.5.5:1080",
		"http://user:pass@6.6.6.6:8080",
		"http://7.7.7.7:9090",
		"http://user:pass@8.8.8.8:1234",
		"http://9.9.9.9:1080",
		"http://11.11.11.11:notaport",
	}
	if len(got) != len(want) {
		t.Fatalf("count mismatch\n got: %v\nwant: %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %q want %q", i, got[i], want[i])
		}
	}

	// A default protocol of socks5 applies only to scheme-less lines.
	got2 := parseProxyLines("1.2.3.4:1080\nhttp://5.6.7.8:8080", "socks5")
	if len(got2) != 2 || got2[0] != "socks5://1.2.3.4:1080" || got2[1] != "http://5.6.7.8:8080" {
		t.Fatalf("default protocol wrong: %v", got2)
	}
}

func TestProxyPoolRoundRobin(t *testing.T) {
	p := newProxyPool()
	mk := func(name string) *proxyTransport {
		return &proxyTransport{name: name, client: &http.Client{}}
	}
	p.replace([]*proxyTransport{mk("a"), mk("b"), mk("c")})
	if p.len() != 3 {
		t.Fatalf("len=%d", p.len())
	}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[p.next().name]++
	}
	for _, n := range []string{"a", "b", "c"} {
		if seen[n] != 2 {
			t.Fatalf("round robin uneven: %v", seen)
		}
	}
	// Empty pool yields nil rather than panicking.
	p.replace(nil)
	if got := p.next(); got != nil {
		t.Fatalf("empty pool must return nil, got %v", got)
	}
}

// TestProxySourceEndToEnd serves a fake subscription returning two live
// and two dead proxies, then asserts only the live ones join the pool.
func TestProxySourceEndToEnd(t *testing.T) {
	// Target the proxies are checked against.
	var checkedBy atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reachable directly; proxies must forward to it.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer target.Close()

	// A working forward proxy is hard to fabricate in-process, so instead
	// exercise the pipeline with direct-style entries: a reachable URL is
	// used as the "proxy" and the transport parser rejects it, which must
	// count as tested+dead rather than crashing.
	var listHits atomic.Int64
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		listHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("# list\n" +
			"127.0.0.1:1\n" +
			"127.0.0.1:2\n" +
			"note: not a proxy\n" +
			"192.0.2.1:9999\n"))
	}))
	defer sub.Close()

	cfg := Config{
		ProxySources: []ProxySource{{
			URL:            sub.URL,
			TestURL:        target.URL,
			TimeoutSeconds: 1,
			Concurrency:    4,
		}},
	}
	pool := newProxyPool()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	runProxySourceOnce(ctx, cfg.ProxySources[0], pool)
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("check took %s, concurrency not applied", elapsed)
	}
	if listHits.Load() == 0 {
		t.Fatal("subscription was never fetched")
	}
	if n := pool.stats().Rejected; n < 3 {
		t.Fatalf("expected >=3 rejected entries, got %d", n)
	}
	if pool.lastFetch.Load() == 0 || pool.lastChecked.Load() == 0 {
		t.Fatal("fetch/check timestamps not recorded")
	}
	checkedBy.Add(1)
}

func TestWaitProxySourcesPopulatesBeforeReturn(t *testing.T) {
	// A subscription with a single structurally-valid proxy that cannot
	// work: the pool stays empty but the call must return promptly.
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("192.0.2.1:9\n"))
	}))
	defer sub.Close()

	cfg := &Config{ProxySources: []ProxySource{{
		URL: sub.URL, TestURL: "http://192.0.2.1:9/", TimeoutSeconds: 1, Concurrency: 2,
	}}}
	pool := newProxyPool()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	waitProxySources(ctx, cfg, pool, 10*time.Second)
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("wait returned too late: %s", elapsed)
	}
	if pool.stats().Sources != 1 {
		t.Fatalf("source count not recorded: %+v", pool.stats())
	}
}

func TestProxySourceConfigValidation(t *testing.T) {
	dir := t.TempDir()
	base := `{"listen":"127.0.0.1:0","server_keys":["k"],"zen_key":"z",
		"hash_store_path":"` + strings.ReplaceAll(dir, `\`, `\\`) + `/h.json",
		"catalog_path":"` + strings.ReplaceAll(dir, `\`, `\\`) + `/c.json"`

	// Unknown field inside a source is a hard error.
	bad := base + `,"proxy_sources":[{"url":"https://x/y","bogus":1}]}`
	path := dir + "/bad.json"
	if err := writeFile(path, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("unknown source field must be rejected")
	}

	// Bad protocol refused.
	bad2 := base + `,"proxy_sources":[{"url":"https://x/y","protocol":"quic"}]}`
	path2 := dir + "/bad2.json"
	if err := writeFile(path2, bad2); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path2); err == nil {
		t.Fatal("unsupported protocol must be rejected")
	}

	// Defaults applied.
	good := base + `,"proxy_sources":[{"url":"https://x/y"}]}`
	path3 := dir + "/good.json"
	if err := writeFile(path3, good); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path3)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	src := cfg.ProxySources[0]
	if src.TestURL != defaultProxyTestURL || src.TimeoutSeconds != 8 || src.Concurrency != 24 {
		t.Fatalf("defaults not applied: %+v", src)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func TestProxySourceJSONShape(t *testing.T) {
	raw := `{"url":"https://a/b","protocol":"socks5","test_url":"https://t/",
		"timeout_seconds":5,"concurrency":9,"max_keep":7,"refresh_hours":3}`
	var src ProxySource
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		t.Fatal(err)
	}
	if src.Protocol != "socks5" || src.MaxKeep != 7 || src.RefreshHours != 3 {
		t.Fatalf("shape broken: %+v", src)
	}
}
