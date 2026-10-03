package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// signalingWriter records body writes and lets a test wait for the first
// one without racing the guard's reader goroutine.
type signalingWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    bytes.Buffer
	status  int
	writes  chan struct{}
	flushes int
}

func newSignalingWriter() *signalingWriter {
	return &signalingWriter{header: http.Header{}, writes: make(chan struct{}, 32)}
}

func (w *signalingWriter) Header() http.Header { return w.header }
func (w *signalingWriter) WriteHeader(c int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = c
	}
	w.mu.Unlock()
}
func (w *signalingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.body.Write(b)
	w.mu.Unlock()
	select {
	case w.writes <- struct{}{}:
	default:
	}
	return len(b), nil
}
func (w *signalingWriter) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
}
func (w *signalingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func pipeResponse() (*http.Response, *io.PipeWriter) {
	pr, pw := io.Pipe()
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       pr,
	}, pw
}

const createdFrame = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n"
const textFrame = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
const moreFrame = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\" there\"}\n\n"
const doneFrame = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"

// TestStreamGuardedLiveNotBuffered is the regression guard: the first
// content must reach the client long before the stream ends.
func TestStreamGuardedLiveNotBuffered(t *testing.T) {
	srv := verifiedTestServer(t, "http://example.invalid")
	srv.cfg.EmptyGuardHold = time.Minute // force the content path
	resp, pw := pipeResponse()
	w := newSignalingWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.streamGuarded(w, resp, ProtoResponses, ProtoResponses, "m", "/v1/responses",
			time.Now(), "r", "s", "p", false)
	}()

	if _, err := pw.Write([]byte(createdFrame + textFrame)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.writes:
		// First content arrived while the upstream stream is still open.
	case <-time.After(2 * time.Second):
		t.Fatal("first content was buffered until stream end")
	}
	if got := w.String(); !strings.Contains(got, "hi") {
		t.Fatalf("first write must carry content, got %q", got)
	}

	// The rest of the stream keeps flowing.
	if _, err := pw.Write([]byte(moreFrame + doneFrame)); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	<-done
	if got := w.String(); !strings.Contains(got, " there") {
		t.Fatalf("later frames must be relayed, got %q", got)
	}
}

// TestStreamGuardedHoldsCreatedUntilContent: a non-content prefix is not
// written until a content event proves the reply is usable.
func TestStreamGuardedHoldsCreatedUntilContent(t *testing.T) {
	srv := verifiedTestServer(t, "http://example.invalid")
	srv.cfg.EmptyGuardHold = time.Minute
	resp, pw := pipeResponse()
	w := newSignalingWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.streamGuarded(w, resp, ProtoResponses, ProtoResponses, "m", "/v1/responses",
			time.Now(), "r", "s", "p", false)
	}()

	if _, err := pw.Write([]byte(createdFrame)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := w.String(); got != "" {
		t.Fatalf("created-only prefix must not be written yet, got %q", got)
	}
	if _, err := pw.Write([]byte(textFrame)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("content never flushed")
	}
	_ = pw.Close()
	<-done
	if got := w.String(); !strings.Contains(got, "hi") {
		t.Fatalf("committed body must include content, got %q", got)
	}
}

// TestStreamGuardedEmptyRetryable: a stream that ends with no content
// writes nothing so the caller can retry it.
func TestStreamGuardedEmptyRetryable(t *testing.T) {
	srv := verifiedTestServer(t, "http://example.invalid")
	resp, pw := pipeResponse()
	w := newSignalingWriter()
	_ = pw.CloseWithError(io.EOF)

	committed, empty, err := srv.streamGuarded(w, resp, ProtoResponses, ProtoResponses, "m",
		"/v1/responses", time.Now(), "r", "s", "p", false)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if committed || !empty {
		t.Fatalf("want committed=false empty=true, got %v %v", committed, empty)
	}
	if w.String() != "" {
		t.Fatalf("empty reply must not reach the client, got %q", w.String())
	}
}

// TestStreamGuardedForceDeliversEmpty: the last attempt forwards the
// empty reply instead of failing the client.
func TestStreamGuardedForceDeliversEmpty(t *testing.T) {
	srv := verifiedTestServer(t, "http://example.invalid")
	resp, pw := pipeResponse()
	w := newSignalingWriter()
	go func() {
		_, _ = pw.Write([]byte(createdFrame + doneFrame))
		_ = pw.Close()
	}()

	committed, empty, err := srv.streamGuarded(w, resp, ProtoResponses, ProtoResponses, "m",
		"/v1/responses", time.Now(), "r", "s", "p", true)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !committed || !empty {
		t.Fatalf("want committed=true empty=true, got %v %v", committed, empty)
	}
	if !strings.Contains(w.String(), "response.created") {
		t.Fatalf("forced delivery must carry the empty stream, got %q", w.String())
	}
}

// TestStreamGuardedHoldDeadlineCommits: a reasoning-only prefix (no
// content) is flushed once the hold deadline passes, so the client never
// waits forever for a first event.
func TestStreamGuardedHoldDeadlineCommits(t *testing.T) {
	srv := verifiedTestServer(t, "http://example.invalid")
	srv.cfg.EmptyGuardHold = 40 * time.Millisecond
	resp, pw := pipeResponse()
	w := newSignalingWriter()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.streamGuarded(w, resp, ProtoResponses, ProtoResponses, "m", "/v1/responses",
			time.Now(), "r", "s", "p", false)
	}()
	if _, err := pw.Write([]byte(createdFrame)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.writes:
	case <-time.After(2 * time.Second):
		t.Fatal("guard never flushed after the hold deadline")
	}
	_ = pw.Close()
	<-done
}

// TestForwardStreamingRetriesEmpty exercises the retry loop end to end:
// two empty upstream answers, then a good one; the client only sees the
// good reply.
func TestForwardStreamingRetriesEmpty(t *testing.T) {
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
	rec := httptest.NewRecorder()
	srv.forwardStreaming(context.Background(), rec, req, "/v1/chat/completions",
		[]byte(`{}`), nil, ProtoChat, ProtoChat, "m", time.Now(), "", "r", "s", "p")

	if hits != 3 {
		t.Fatalf("want 3 upstream hits, got %d", hits)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"hi"`) {
		t.Fatalf("client must receive the good reply, got %q", body)
	}
	if strings.Count(body, `"stop"`) == 0 {
		t.Fatalf("good reply must be complete, got %q", body)
	}
}

func TestFrameScanner(t *testing.T) {
	var s frameScanner
	if got := s.push([]byte("data: a\n")); len(got) != 0 {
		t.Fatalf("partial frame must be held, got %d", len(got))
	}
	got := s.push([]byte("\ndata: b\n\n"))
	if len(got) != 2 {
		t.Fatalf("want 2 frames, got %d", len(got))
	}
	if string(got[0]) != "data: a" || string(got[1]) != "data: b" {
		t.Fatalf("bad frames: %q %q", got[0], got[1])
	}
	// CRLF, including a split across pushes.
	var c frameScanner
	_ = c.push([]byte("data: x\r"))
	f2 := c.push([]byte("\n\r\n"))
	if len(f2) != 1 || string(f2[0]) != "data: x" {
		t.Fatalf("CRLF split frame not reassembled: %q", f2)
	}
}
