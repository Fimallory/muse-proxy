package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustObj(t *testing.T, s string) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return p
}

func toolNames(t *testing.T, payload map[string]any) []string {
	t.Helper()
	raw, ok := payload["tools"].([]any)
	if !ok {
		t.Fatalf("tools not an array: %T", payload["tools"])
	}
	var out []string
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("tool not an object: %v", it)
		}
		name, _ := m["name"].(string)
		out = append(out, name)
		if _, ok := m["parameters"].(map[string]any); !ok {
			t.Fatalf("tool %q missing parameters object", name)
		}
	}
	return out
}

func TestEnsureToolsAbsent(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","stream":true}`)
	if !ensureResponseTools(p) {
		t.Fatal("expected change")
	}
	names := toolNames(t, p)
	if len(names) != 5 {
		t.Fatalf("want 5 tools, got %v", names)
	}
	for _, want := range coreToolNames {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing core tool %q", want)
		}
	}
}

func TestEnsureToolsClientPriority(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","stream":true,"tools":[
		{"type":"function","name":"weather","description":"Real tool",
		 "parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
		{"type":"function","name":"bash"}]}`)
	if !ensureResponseTools(p) {
		t.Fatal("expected change (bash params + missing cores)")
	}
	raw := p["tools"].([]any)
	// Client tools keep position and content.
	first := raw[0].(map[string]any)
	if first["name"] != "weather" || first["description"] != "Real tool" {
		t.Fatalf("client tool was rewritten: %v", first)
	}
	params := first["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["city"]; !ok {
		t.Fatalf("client params were clobbered: %v", params)
	}
	// bash got default params.
	second := raw[1].(map[string]any)
	if _, ok := second["parameters"].(map[string]any); !ok {
		t.Fatalf("bash params not filled: %v", second)
	}
	// Missing cores appended.
	names := toolNames(t, p)
	if len(names) != 2+4 {
		t.Fatalf("want 6 tools, got %v", names)
	}
}

func TestEnsureToolsUntouched(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","stream":true,"tools":[
		{"type":"function","name":"bash","parameters":{"type":"object","properties":{}}},
		{"type":"function","name":"edit","parameters":{"type":"object","properties":{}}},
		{"type":"function","name":"glob","parameters":{"type":"object","properties":{}}},
		{"type":"function","name":"grep","parameters":{"type":"object","properties":{}}},
		{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]}`)
	if ensureResponseTools(p) {
		t.Fatal("expected no change")
	}
}

func TestEnsureToolsDropsJunk(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","stream":true,"tools":[null,{}, {"type":"function","name":"bash"}]}`)
	if !ensureResponseTools(p) {
		t.Fatal("expected change")
	}
	names := toolNames(t, p)
	// {} has no name but is an object: kept, then cores appended.
	// null dropped. bash patched. edit/glob/grep/read appended.
	if len(names) != 1+1+4 {
		t.Fatalf("want 6 entries, got %v", names)
	}
}

func TestWantsCollapse(t *testing.T) {
	if !wantsCollapse(mustObj(t, `{"stream":false}`)) {
		t.Fatal("stream:false should collapse")
	}
	if !wantsCollapse(mustObj(t, `{}`)) {
		t.Fatal("absent stream should collapse")
	}
	if wantsCollapse(mustObj(t, `{"stream":true}`)) {
		t.Fatal("stream:true should not collapse")
	}
}

var cannedSSE = "event: response.created\n" +
	`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_abc","object":"response","created_at":1700000000,"status":"in_progress","model":"muse-spark-1.3-contributor-free"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","delta":"4"}` + "\n\n" +
	"event: response.output_text.done\n" +
	`data: {"type":"response.output_text.done","text":"4"}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_abc","object":"response","created_at":1700000000,"status":"completed","model":"muse-spark-1.3-contributor-free","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"4","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}}` + "\n\n"

func TestCollapseCompleted(t *testing.T) {
	out, status := collapseSSE([]byte(cannedSSE), "m", "")
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, out)
	}
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if p["id"] != "resp_abc" || p["status"] != "completed" {
		t.Fatalf("wrong object: %s", out)
	}
	usage, ok := p["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(12) {
		t.Fatalf("usage lost: %s", out)
	}
}

func TestCollapseTruncated(t *testing.T) {
	// No completed event: synthesize from deltas.
	partial := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_x","model":"m"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hel"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"lo"}` + "\n\n"
	out, status := collapseSSE([]byte(partial), "m", "")
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if p["status"] != "incomplete" {
		t.Fatalf("want incomplete, got %s", out)
	}
	s := string(out)
	if !strings.Contains(s, "hello") {
		t.Fatalf("text lost: %s", s)
	}
}

func TestCollapseFailed(t *testing.T) {
	body := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_f","object":"response","status":"failed","error":{"message":"boom"}}}` + "\n\n"
	out, status := collapseSSE([]byte(body), "m", "")
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(string(out), `"status":"failed"`) {
		t.Fatalf("failed status lost: %s", out)
	}
}

func TestFailoverClassification(t *testing.T) {
	if !shouldFailover(429, nil) || !shouldFailover(500, nil) || !shouldFailover(503, nil) {
		t.Fatal("429/5xx must fail over")
	}
	if !shouldFailover(0, errTest) {
		t.Fatal("transport error must fail over")
	}
	for _, s := range []int{400, 401, 403, 404, 422} {
		if shouldFailover(s, nil) {
			t.Fatalf("%d must not fail over", s)
		}
		if !deterministic4xx(s) {
			t.Fatalf("%d must be deterministic", s)
		}
	}
	if deterministic4xx(429) || deterministic4xx(500) {
		t.Fatal("429/500 are not deterministic")
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "test" }

// TestFailoverDirect429ToPool wires a 429 direct route and a 200 pool route
// and asserts the request lands on the pool exactly once per route.
func TestFailoverDirect429ToPool(t *testing.T) {
	var directHits, poolHits int
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits++
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer direct.Close()
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		poolHits++
		if r.RequestURI != "http://example.invalid/v1/responses" && !strings.HasSuffix(r.URL.Path, "/v1/responses") {
			t.Errorf("unexpected pool path %q", r.RequestURI)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	}))
	defer pool.Close()

	cfg := &Config{
		Listen: "127.0.0.1:0", ServerKeys: []string{"k"}, ZenKey: "z",
		Upstream: direct.URL, Proxies: []string{pool.URL},
		PreferDirect: true, DirectCooldownSeconds: 120, PoolMaxAttempts: 3,
		HashTTLDays: 3, HashMaxEntries: 100, HashPrefixChars: 100,
		RequestTimeoutSeconds: 30,
	}
	cfg.HashTTL = time.Hour
	cfg.RequestTimeout = 30 * time.Second
	cfg.DirectCooldown = 120 * time.Second
	store, err := openStore(t.TempDir()+"/h.json", time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	srv, err := newServer(cfg, store, openCatalog(t.TempDir()+"/c.json", "http://example.invalid", time.Hour), newProxyPool())
	if err != nil {
		t.Fatal(err)
	}

	mkReq := func() *http.Request {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"m"}`))
		r.Header.Set("Authorization", "Bearer k")
		return r
	}
	resp, err := srv.roundTrip(context.Background(), mkReq(), "/v1/responses", []byte(`{"model":"m"}`), nil)
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if directHits != 1 || poolHits != 1 {
		t.Fatalf("want 1 direct + 1 pool hit, got %d + %d", directHits, poolHits)
	}

	// Direct is now banned: the next request must skip it.
	resp2, err := srv.roundTrip(context.Background(), mkReq(), "/v1/responses", []byte(`{"model":"m"}`), nil)
	if err != nil {
		t.Fatalf("roundTrip2: %v", err)
	}
	resp2.Body.Close()
	if directHits != 1 || poolHits != 2 {
		t.Fatalf("ban not honored: direct=%d pool=%d", directHits, poolHits)
	}
}

// TestStoreTTL verifies expiry without waiting days.
func TestStoreTTL(t *testing.T) {
	store, err := openStore(t.TempDir()+"/h.json", time.Millisecond, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	store.put("abc", "ses_x", "prj_y")
	time.Sleep(5 * time.Millisecond)
	if _, ok := store.get("abc"); ok {
		t.Fatal("expired entry must not match")
	}
	if n := store.count(); n != 0 {
		t.Fatalf("expired entry must be purged, count=%d", n)
	}
}
