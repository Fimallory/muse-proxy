package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// stream_guard.go relays streaming replies while hiding empty ones.
//
// The first empty-reply guard buffered the whole upstream answer before
// writing a single byte, which broke SSE liveness: a client waiting for
// the first event saw nothing until the generation finished and gave up
// with "stream first event timeout". This file restores incremental
// delivery. The response prefix is held only until the reply is proven
// to carry content (text or a tool call), the guard deadline passes, or
// the stream ends. Because nothing is written before a commit, an empty
// reply can still be retried on a fresh connection without the client
// ever seeing it.

// frameScanner splits an SSE byte stream into complete frames as chunks
// arrive, tolerating CRLF line endings and frames that span reads.
type frameScanner struct{ buf []byte }

func (f *frameScanner) push(b []byte) [][]byte {
	if len(b) > 0 {
		f.buf = append(f.buf, b...)
		if bytes.IndexByte(f.buf, '\r') >= 0 {
			f.buf = bytes.ReplaceAll(f.buf, []byte("\r\n"), []byte("\n"))
		}
	}
	var out [][]byte
	for {
		i := bytes.Index(f.buf, []byte("\n\n"))
		if i < 0 {
			break
		}
		out = append(out, f.buf[:i])
		f.buf = f.buf[i+2:]
	}
	return out
}

// eventHasContent reports whether a normalized event makes a reply
// non-empty. Reasoning alone does not count: from the client's point of
// view a reply with no text and no tool call is useless.
func eventHasContent(ev uevent) bool {
	switch ev.kind {
	case "text":
		return strings.TrimSpace(ev.text) != ""
	case "tool":
		return ev.toolName != "" || ev.toolDone
	}
	return false
}

// streamGuarded relays resp to w, holding the prefix until the reply is
// proven non-empty. See the file comment for the rationale.
//
// It returns:
//
//	committed - bytes reached w; the caller must not retry.
//	empty     - nothing was written because the reply carried no
//	            content (retryable), or (force) the empty reply was
//	            delivered after the retry budget ran out.
//	err       - transport/read error before commit (retryable).
func (s *Server) streamGuarded(
	w http.ResponseWriter,
	resp *http.Response,
	native, client Protocol,
	model, upPath string,
	start time.Time,
	rid, session, project string,
	force bool,
) (committed, empty bool, err error) {
	cross := native != client
	var tc *transcoder
	if cross {
		tc = newTranscoder(client, model)
	}

	// Read the upstream body on a separate goroutine so the hold
	// deadline can fire without blocking on a slow first byte.
	done := make(chan struct{})
	defer close(done)
	type chunk struct {
		data []byte
		err  error
	}
	ch := make(chan chunk, 16)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, e := resp.Body.Read(buf)
			var cp []byte
			if n > 0 {
				cp = append([]byte(nil), buf[:n]...)
			}
			if n > 0 || e != nil {
				select {
				case ch <- chunk{data: cp, err: e}:
				case <-done:
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()

	fl, _ := w.(http.Flusher)
	headersWritten := false
	var held []byte // downstream bytes not yet flushed

	writeHeaders := func() {
		if headersWritten {
			return
		}
		headersWritten = true
		copyHeaderStrip(w.Header(), resp.Header)
		w.Header().Del("Content-Length")
		if cross || w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		setEchoHeaders(w, rid, session, project)
		w.WriteHeader(resp.StatusCode)
	}

	// emit queues downstream bytes until the first commit, then writes
	// them straight through.
	emit := func(b []byte) {
		if len(b) == 0 || err != nil {
			return
		}
		if !committed {
			held = append(held, b...)
			return
		}
		writeHeaders()
		if _, e := w.Write(b); e != nil {
			err = e
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
	commit := func() {
		if committed {
			return
		}
		committed = true
		writeHeaders()
		if len(held) > 0 {
			if _, e := w.Write(held); e != nil {
				err = e
			}
			held = nil
			if fl != nil {
				fl.Flush()
			}
		}
	}

	scanner := &frameScanner{}
	hasContent := false
	abort := false // upstream reported an error before any content

	process := func(frame []byte) {
		data := frameData(frame)
		if data == "" || data == "[DONE]" {
			if !cross {
				emit(frameBytes(frame))
			}
			return
		}
		var ev map[string]any
		if json.Unmarshal([]byte(data), &ev) != nil {
			if !cross {
				emit(frameBytes(frame))
			}
			return
		}
		for _, ue := range parseOneEvent(native, ev) {
			if ue.kind == "error" && !hasContent {
				// An error before any content is an empty reply: let
				// the caller retry it on a fresh connection.
				abort = true
				return
			}
			if eventHasContent(ue) {
				hasContent = true
			}
			if cross {
				emit(tc.push(ue))
			}
		}
		if !cross {
			emit(frameBytes(frame))
		}
	}

	var hold <-chan time.Time
	if d := s.cfg.EmptyGuardHold; d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		hold = t.C
	}

	for {
		select {
		case msg := <-ch:
			if len(msg.data) > 0 {
				for _, frame := range scanner.push(msg.data) {
					process(frame)
					if abort && !committed {
						return false, true, nil
					}
				}
				if hasContent && !committed {
					commit()
				}
				if err != nil {
					return committed, false, err
				}
			}
			if msg.err != nil {
				if !committed {
					switch {
					case hasContent:
						commit()
					case msg.err == io.EOF:
						if force {
							// Budget is out: deliver the empty reply
							// rather than a hard failure.
							commit()
							empty = true
						} else {
							return false, true, nil
						}
					default:
						return false, false, msg.err
					}
				}
				if cross {
					emit(tc.flush())
				}
				return committed, empty, err
			}
		case <-hold:
			hold = nil
			// First token is slow: stop hiding the stream so the
			// client keeps receiving events.
			commit()
		}
	}
}

// frameBytes re-emits a raw frame with a trailing blank line.
func frameBytes(frame []byte) []byte {
	out := make([]byte, 0, len(frame)+2)
	out = append(out, frame...)
	return append(out, '\n', '\n')
}
