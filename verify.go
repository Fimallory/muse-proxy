package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// verify.go implements the empty-reply guard: a 2xx upstream reply that
// carries no text and no tool calls is never passed to the client.
// Instead the request is re-sent internally (fresh connection, so a
// rotating pool lands on another egress node) until a usable reply
// arrives or the budget runs out. The last reply is forwarded even if
// still empty, so behavior degrades transparently instead of failing.

// verifiedResp is one fully-buffered upstream answer.
type verifiedResp struct {
	status int
	header http.Header
	body   []byte
}

// fetchVerified runs the egress strategy and returns the first non-empty
// 2xx reply, retrying empty ones internally. Non-2xx replies pass
// through untouched (they are errors, not empty answers).
func (s *Server) fetchVerified(ctx context.Context, r *http.Request, upPath string, body []byte, extra map[string]string, native Protocol, model string) (*verifiedResp, error) {
	tries := 1
	if s.cfg.RetryEmpty {
		tries += s.cfg.MaxEmptyRetries
	}
	var last *verifiedResp
	for i := 0; i < tries; i++ {
		resp, err := s.roundTrip(ctx, r, upPath, body, extra)
		if err != nil {
			if i == tries-1 {
				return nil, err
			}
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, collapseMaxBytes))
		resp.Body.Close()
		if err != nil {
			if i == tries-1 {
				return nil, err
			}
			continue
		}
		vr := &verifiedResp{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}
		if resp.StatusCode/100 != 2 {
			return vr, nil
		}
		if !s.cfg.RetryEmpty || !isEmptyReply(native, raw) {
			return vr, nil
		}
		last = vr
		log.Printf("empty %s reply for %s, retrying internally (%d/%d)",
			native, model, i+1, tries)
		time.Sleep(500 * time.Millisecond)
	}
	if last == nil {
		last = &verifiedResp{status: 502, body: []byte(`{"error":{"message":"upstream request failed"}}`)}
	}
	return last, nil
}

// isEmptyReply reports whether a 2xx upstream body carries no usable
// content: no non-blank text and no tool calls. An error event inside
// the stream also counts as empty (failed answer, worth a retry).
func isEmptyReply(native Protocol, raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	if trimmed[0] == '{' {
		var p map[string]any
		if err := json.Unmarshal(trimmed, &p); err != nil {
			return true
		}
		ir := decodeResponse(native, p, "")
		if strings.TrimSpace(ir.Text) != "" {
			return false
		}
		for _, t := range ir.Tools {
			if t.Name != "" {
				return false
			}
		}
		return true
	}
	return isEmptyEvents(parseSSEEvents(native, trimmed))
}

// isEmptyEvents scans normalized stream events for usable content.
// Reasoning alone does not count: from the client's perspective a reply
// with zero text and zero tool calls is useless.
func isEmptyEvents(events []uevent) bool {
	if len(events) == 0 {
		return true
	}
	for _, ev := range events {
		switch ev.kind {
		case "error":
			return true
		case "text":
			if strings.TrimSpace(ev.text) != "" {
				return false
			}
		case "tool":
			if ev.toolName != "" || ev.toolDone {
				return false
			}
		}
	}
	return true
}
