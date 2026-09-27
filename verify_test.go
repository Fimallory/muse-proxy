package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func emptySSEChat() []byte {
	return []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
}

func fullSSEChat() []byte {
	return []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n")
}

func TestIsEmptyReplySSE(t *testing.T) {
	if !isEmptyReply(ProtoChat, emptySSEChat()) {
		t.Fatal("blank chat stream must be empty")
	}
	if isEmptyReply(ProtoChat, fullSSEChat()) {
		t.Fatal("chat stream with text must not be empty")
	}
	if !isEmptyReply(ProtoChat, []byte("")) {
		t.Fatal("zero bytes must be empty")
	}
	if !isEmptyReply(ProtoChat, []byte("   \n  ")) {
		t.Fatal("whitespace must be empty")
	}
	// Whitespace-only text is still empty.
	ws := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"  \\n \"}}]}\n\n")
	if !isEmptyReply(ProtoChat, ws) {
		t.Fatal("whitespace-only content must be empty")
	}
	// Tool call without text counts as content.
	tc := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"bash\"}}]}}]}\n\n")
	if isEmptyReply(ProtoChat, tc) {
		t.Fatal("tool call must count as content")
	}
	// Error events fail the reply.
	er := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n\n")
	if !isEmptyReply(ProtoResponses, er) {
		t.Fatal("error event must count as empty")
	}
}

func TestIsEmptyReplyResponses(t *testing.T) {
	// Reasoning-only responses stream: no usable answer.
	ro := []byte("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n")
	if !isEmptyReply(ProtoResponses, ro) {
		t.Fatal("reasoning-only must be empty")
	}
	ok := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n")
	if isEmptyReply(ProtoResponses, ok) {
		t.Fatal("text reply must not be empty")
	}
}

func TestIsEmptyReplyJSON(t *testing.T) {
	if !isEmptyReply(ProtoChat, []byte(`{"choices":[{"message":{"role":"assistant","content":""}}]}`)) {
		t.Fatal("empty chat JSON must be empty")
	}
	if isEmptyReply(ProtoChat, []byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}]}`)) {
		t.Fatal("non-empty chat JSON must pass")
	}
	if !isEmptyReply(ProtoResponses, []byte(`{"status":"incomplete","output":[]}`)) {
		t.Fatal("empty responses JSON must be empty")
	}
	// Unparseable 2xx bodies are retried rather than passed through blind.
	if !isEmptyReply(ProtoChat, []byte(`not json at all`)) {
		t.Fatal("garbage 2xx must be treated as empty")
	}
}

// TestFetchVerifiedRetriesEmpty wires a stub upstream that answers empty
// twice, then good. The client must only ever see the good reply.
func TestFetchVerifiedRetriesEmpty(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		if hits < 3 {
			_, _ = w.Write(emptySSEChat())
			return
		}
		_, _ = w.Write(fullSSEChat())
	}))
	defer up.Close()

	srv := verifiedTestServer(t, up.URL)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	res, err := srv.fetchVerified(context.Background(), req, "/v1/chat/completions",
		[]byte(`{}`), nil, ProtoChat, "m")
	if err != nil {
		t.Fatalf("fetchVerified: %v", err)
	}
	if res.status != 200 {
		t.Fatalf("status=%d", res.status)
	}
	if hits != 3 {
		t.Fatalf("want 3 upstream hits, got %d", hits)
	}
	if isEmptyReply(ProtoChat, res.body) {
		t.Fatal("returned body must be the non-empty one")
	}
}

// TestFetchVerifiedGivesUp forwards the last reply once the budget is out.
func TestFetchVerifiedGivesUp(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(emptySSEChat())
	}))
	defer up.Close()

	srv := verifiedTestServer(t, up.URL) // MaxEmptyRetries=2 -> 3 tries
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	res, err := srv.fetchVerified(context.Background(), req, "/v1/chat/completions",
		[]byte(`{}`), nil, ProtoChat, "m")
	if err != nil {
		t.Fatalf("fetchVerified: %v", err)
	}
	if hits != 3 {
		t.Fatalf("want 3 upstream hits, got %d", hits)
	}
	if !isEmptyReply(ProtoChat, res.body) {
		t.Fatal("expected the last (empty) reply to be forwarded")
	}
}

// TestFetchVerifiedSkipsErrors: non-2xx passes through with no retry.
func TestFetchVerifiedSkipsErrors(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"slow"}`))
	}))
	defer up.Close()

	srv := verifiedTestServer(t, up.URL)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	res, err := srv.fetchVerified(context.Background(), req, "/v1/chat/completions",
		[]byte(`{}`), nil, ProtoChat, "m")
	if err != nil {
		t.Fatalf("fetchVerified: %v", err)
	}
	if res.status != 429 || hits != 1 {
		t.Fatalf("error must pass through once: status=%d hits=%d", res.status, hits)
	}
}

// TestFetchVerifiedDisabled returns the first reply untouched.
func TestFetchVerifiedDisabled(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(emptySSEChat())
	}))
	defer up.Close()

	srv := verifiedTestServer(t, up.URL)
	srv.cfg.RetryEmpty = false
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	res, err := srv.fetchVerified(context.Background(), req, "/v1/chat/completions",
		[]byte(`{}`), nil, ProtoChat, "m")
	if err != nil {
		t.Fatalf("fetchVerified: %v", err)
	}
	if hits != 1 || !isEmptyReply(ProtoChat, res.body) {
		t.Fatalf("disabled mode must forward first reply: hits=%d", hits)
	}
}

func verifiedTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cfg := &Config{
		Listen: "127.0.0.1:0", ServerKeys: []string{"k"}, ZenKey: "z",
		Upstream: upstream, Proxies: []string{"direct"},
		PreferDirect: true, DirectCooldownSeconds: 0, PoolMaxAttempts: 1,
		RetryEmpty: true, MaxEmptyRetries: 2,
		HashTTLDays: 3, HashMaxEntries: 100, HashPrefixChars: 100,
		RequestTimeoutSeconds: 30,
	}
	cfg.HashTTL = time.Hour
	cfg.RequestTimeout = 30 * time.Second
	store, err := openStore(t.TempDir()+"/h.json", time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })
	cat := openCatalog(t.TempDir()+"/c.json", "http://example.invalid", time.Hour)
	srv, err := newServer(cfg, store, cat, newProxyPool())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}
