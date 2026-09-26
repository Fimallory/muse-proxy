package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// defaultUA mirrors a real OpenCode client.
const defaultUA = "opencode/1.18.31 (linux amd64; go1.24.0)"

// maxBody caps forwarded request bodies at 32 MiB, like other 2api gateways.
const maxBody = 32 << 20

var hopHeaders = map[string]bool{
	"connection": true, "proxy-connection": true, "keep-alive": true,
	"proxy-authenticate": true, "proxy-authorization": true,
	"te": true, "trailer": true, "transfer-encoding": true,
	"upgrade": true, "content-length": true,
}

type proxyTransport struct {
	name   string
	client *http.Client
}

// Server is the forwarding gateway.
type Server struct {
	cfg      *Config
	store    *hashStore
	catalog  *catalog
	direct   *proxyTransport
	pools    []*proxyTransport
	legacy   []*proxyTransport // prefer_direct=false: round-robin over all
	rr       atomic.Uint64
	banUntil atomic.Int64 // direct egress skipped while now < banUntil
}

func newServer(cfg *Config, store *hashStore, cat *catalog) (*Server, error) {
	s := &Server{cfg: cfg, store: store, catalog: cat}
	mkTransport := func(raw string) (*proxyTransport, error) {
		tr := &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			ExpectContinueTimeout: 1 * time.Second,
		}
		if raw == "direct" {
			tr.Proxy = nil
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, err
			}
			tr.Proxy = http.ProxyURL(u)
		}
		if raw != "direct" && !cfg.ReuseProxyConns {
			// A rotating pool assigns an egress node per connection, so a
			// pooled keep-alive tunnel would pin every request to one node.
			// HTTP/2 multiplexes too, so it goes as well, and ALPN is pinned
			// to http/1.1 or the server answers with h2 frames on an h1
			// connection (malformed response).
			tr.DisableKeepAlives = true
			tr.ForceAttemptHTTP2 = false
			tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			tlsCfg := tr.TLSClientConfig
			if tlsCfg == nil {
				tlsCfg = &tls.Config{}
			} else {
				tlsCfg = tlsCfg.Clone()
			}
			tlsCfg.NextProtos = []string{"http/1.1"}
			tr.TLSClientConfig = tlsCfg
			tr.MaxIdleConns = 0
			tr.MaxIdleConnsPerHost = 0
		}
		return &proxyTransport{name: raw, client: &http.Client{Transport: tr}}, nil
	}
	if cfg.PreferDirect {
		d, err := mkTransport("direct")
		if err != nil {
			return nil, err
		}
		s.direct = d
		for _, raw := range cfg.Proxies {
			if raw == "direct" {
				continue
			}
			t, err := mkTransport(raw)
			if err != nil {
				return nil, err
			}
			s.pools = append(s.pools, t)
		}
	} else {
		for _, raw := range cfg.Proxies {
			t, err := mkTransport(raw)
			if err != nil {
				return nil, err
			}
			s.legacy = append(s.legacy, t)
		}
	}
	return s, nil
}

func (s *Server) close() {}

func (s *Server) pickPool() *proxyTransport {
	if len(s.pools) == 1 {
		return s.pools[0]
	}
	return s.pools[int(s.rr.Add(1))%len(s.pools)]
}

// deterministic4xx reports request shapes the upstream will always reject
// regardless of egress: retrying them through another route cannot help.
func deterministic4xx(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusTooManyRequests
}

// shouldFailover reports whether a failed direct attempt is worth retrying
// through the pool: transport errors, rate limits and server errors.
func shouldFailover(status int, err error) bool {
	if err != nil {
		return true
	}
	return status == http.StatusTooManyRequests || status >= 500
}

// banDirect takes the direct route out of rotation for a while:
// a throttled route stays out for DirectCooldown (or longer when the
// upstream names a Retry-After); a broken route gets a brief backoff.
func (s *Server) banDirect(resp *http.Response, err error) {
	var cd time.Duration
	if err != nil {
		cd = 30 * time.Second
	}
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		cd = s.cfg.DirectCooldown
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > cd {
			cd = ra
		}
	}
	if cd > 0 {
		s.banUntil.Store(time.Now().Add(cd).UnixNano())
	}
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, strings.TrimSpace(v)); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func closeResp(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// withAuth gates a handler on the local server keys.
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var cand string
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			cand = strings.TrimSpace(auth[7:])
		} else {
			cand = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		ok := false
		for _, k := range s.cfg.ServerKeys {
			if subtle.ConstantTimeCompare([]byte(cand), []byte(k)) == 1 {
				ok = true
				break
			}
		}
		if !ok {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"message": "unauthorized"}})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	egress := map[string]any{"prefer_direct": s.cfg.PreferDirect}
	if s.cfg.PreferDirect {
		egress["direct_banned"] = time.Now().UnixNano() < s.banUntil.Load()
		egress["pool_routes"] = len(s.pools)
	}
	entries, updated, source := s.catalog.snapshot()
	writeJSON(w, 200, map[string]any{
		"status":  "ok",
		"version": version,
		"hashes":  s.store.count(),
		"egress":  egress,
		"catalog": map[string]any{"models": entries, "updated": updated, "source": source},
	})
}

// handleModels forwards the upstream model list untouched.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	rid := strings.TrimSpace(r.Header.Get("x-opencode-request"))
	if rid == "" {
		rid = RandomID("req", 16)
	}
	s.forward(w, r, "/v1/models", nil, map[string]string{"x-opencode-request": rid}, rid, "", "", "", false)
}

// handleResponses serves the Responses API.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleProto(w, r, ProtoResponses)
}

// handleChat serves the Chat Completions API.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.handleProto(w, r, ProtoChat)
}

// handleMessages serves the Anthropic Messages API.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProto(w, r, ProtoMessages)
}

// handleProto serves one client protocol: resolve the model's native
// protocol from the catalog, convert when they differ, and relay.
func (s *Server) handleProto(w http.ResponseWriter, r *http.Request, clientProto Protocol) {
	origBody, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "cannot read body"}})
		return
	}
	decoded := decodeObj(origBody)
	if decoded == nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "request body must be a JSON object"}})
		return
	}
	model := requestModel(origBody)
	if model == "" {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "missing model"}})
		return
	}
	native := s.catalog.nativeProtocol(model)
	if native == ProtoGoogle {
		writeJSON(w, 400, map[string]any{"error": map[string]any{
			"message": "model " + model + " uses an unsupported native protocol"}})
		return
	}

	// Build the upstream body: convert when protocols differ, otherwise
	// patch tools in place. Keep original bytes when nothing changed.
	var body []byte
	converted := false
	if native != clientProto {
		ir := decodeRequest(clientProto, decoded)
		// The client stream flag travels with the IR.
		up := encodeRequest(native, ir)
		ensureTools(up, native)
		if re, err := marshalNoEscape(up); err == nil {
			body = re
			converted = true
		}
	}
	if body == nil {
		body = origBody
		if ensureTools(decoded, native) {
			if re, err := marshalNoEscape(decoded); err == nil {
				body = re
			}
		}
	}
	_ = converted

	collapse := wantsCollapse(decoded)
	if collapse {
		// The free tier only serves streaming: force it on the wire and
		// fold the stream back below.
		var up map[string]any
		if err := json.Unmarshal(body, &up); err == nil {
			if stream, _ := up["stream"].(bool); !stream {
				up["stream"] = true
				if re, err := marshalNoEscape(up); err == nil {
					body = re
				}
			}
		}
	}

	instr, input := extractTexts(origBody)
	hash := contentHash(instr, input, s.cfg.HashPrefixChars)
	session, project := s.resolveIDs(r, origBody, hash, instr == "" && input == "")

	rid := strings.TrimSpace(r.Header.Get("x-opencode-request"))
	if rid == "" {
		rid = RandomID("req", 16)
	}
	extra := map[string]string{
		"x-opencode-session": session,
		"x-session-affinity": session,
		"X-Session-Id":       session,
		"x-opencode-project": project,
		"x-opencode-request": rid,
		"x-opencode-client":  "cli",
		"Accept":             "application/json, text/event-stream",
		"User-Agent":         defaultUA,
	}
	// Anthropic endpoints authenticate on x-api-key instead of Authorization.
	if native == ProtoMessages {
		extra["x-api-key"] = s.cfg.ZenKey
	}
	s.forwardProto(w, r, native, clientProto, body, extra, rid, session, project, model, collapse)
}

func decodeObj(body []byte) map[string]any {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil || p == nil {
		return nil
	}
	return p
}

func requestModel(body []byte) string {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return ""
	}
	m, _ := p["model"].(string)
	return m
}

// resolveIDs picks session/project with this precedence:
// explicit headers > body metadata > hash-store match > freshly minted.
// The mapping is recorded so the next same-content request reuses it.
func (s *Server) resolveIDs(r *http.Request, body []byte, hash string, textEmpty bool) (session, project string) {
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)

	explicitSess := firstHeader(r,
		"x-opencode-session", "x-session-affinity", "X-Session-Id", "x-session-id")
	metaSess, metaProj := "", ""
	if decoded != nil {
		metaSess = jsonStringAt(decoded, "metadata", "session_id")
		metaProj = jsonStringAt(decoded, "metadata", "project_id")
	}
	explicitProj := strings.TrimSpace(r.Header.Get("x-opencode-project"))

	var stored *hashEntry
	if (explicitSess == "" && metaSess == "") || (explicitProj == "" && metaProj == "") {
		if e, ok := s.store.get(hash); ok {
			cp := *e
			stored = &cp
		}
	}

	switch {
	case explicitSess != "":
		session = CanonicalSessionID(strings.TrimSpace(explicitSess))
	case metaSess != "":
		session = CanonicalSessionID(strings.TrimSpace(metaSess))
	case stored != nil:
		session = stored.Session
	case textEmpty:
		session = CanonicalSessionID("empty:" + RandomID("r", 8))
	default:
		session = CanonicalSessionID("content:" + hash)
	}

	switch {
	case explicitProj != "":
		project = explicitProj
	case metaProj != "":
		project = metaProj
	case stored != nil && stored.Project != "":
		project = stored.Project
	default:
		project = StableID("prj", session)
	}

	if !textEmpty {
		s.store.put(hash, session, project)
	}
	return session, project
}

// forwardProto sends the request upstream in the native protocol and
// relays the response back in the client protocol, converting when they
// differ. Streaming responses are transcoded live; collapse mode folds
// the stream into a single JSON object for stream:false clients.
func (s *Server) forwardProto(w http.ResponseWriter, r *http.Request, native, client Protocol, body []byte, extra map[string]string, rid, session, project, model string, collapse bool) {
	upPath := native.Path()
	if upPath == "" {
		upPath = ProtoChat.Path()
	}
	// One deadline covers every egress attempt AND the body relay. It must
	// stay alive until the response is fully consumed, so it lives here,
	// not inside roundTrip.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	start := time.Now()
	conv := ""
	if native != client {
		conv = string(native) + "->" + string(client) + " "
	}
	resp, err := s.roundTrip(ctx, r, upPath, body, extra)
	if err != nil {
		log.Printf("upstream %s %s error: %v", r.Method, upPath, err)
		writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "upstream request failed"}})
		return
	}
	defer resp.Body.Close()

	if collapse && resp.StatusCode/100 == 2 {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, collapseMaxBytes))
		if err != nil {
			writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "read upstream stream"}})
			return
		}
		var out []byte
		var status int
		if native == client && native == ProtoResponses {
			// Verbatim completed object: highest fidelity.
			out, status = collapseSSE(raw, model, "")
		} else {
			events := parseSSEEvents(native, raw)
			out = eventsToResponseJSON(client, events, model)
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		setEchoHeaders(w, rid, session, project)
		w.WriteHeader(status)
		_, _ = w.Write(out)
		log.Printf("%s %s -> %d %scollapsed %dB %dms sess=%s", r.Method, upPath,
			status, conv, len(out), time.Since(start).Milliseconds(), shortID(session))
		return
	}

	if native == client {
		s.relayStream(w, r, resp, upPath, start, rid, session, project, "")
		return
	}
	// Cross-protocol stream: transcode live.
	s.transcodeStream(w, r, resp, native, client, model, upPath, start, rid, session, project)
}

// forward serves the models path (same-protocol GET relay).
func (s *Server) forward(w http.ResponseWriter, r *http.Request, upPath string, body []byte, extra map[string]string, rid, session, project, model string, collapse bool) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	start := time.Now()
	resp, err := s.roundTrip(ctx, r, upPath, body, extra)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "upstream request failed"}})
		return
	}
	defer resp.Body.Close()
	s.relayStream(w, r, resp, upPath, start, rid, session, project, model)
	_ = collapse
}

// relayStream copies an upstream response through untouched.
func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, resp *http.Response, upPath string, start time.Time, rid, session, project, _ string) {
	copyHeaderStrip(w.Header(), resp.Header)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	setEchoHeaders(w, rid, session, project)
	w.WriteHeader(resp.StatusCode)
	streamCopy(w, resp.Body)
	log.Printf("%s %s -> %d %dms sess=%s", r.Method, upPath, resp.StatusCode,
		time.Since(start).Milliseconds(), shortID(session))
}

// transcodeStream converts a foreign-protocol SSE stream into the client
// protocol incrementally, flushing converted frames as they arrive.
func (s *Server) transcodeStream(w http.ResponseWriter, r *http.Request, resp *http.Response, native, client Protocol, model, upPath string, start time.Time, rid, session, project string) {
	tc := newTranscoder(client, model)
	ct := "text/event-stream"
	if client == ProtoChat || client == ProtoResponses || client == ProtoMessages {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	setEchoHeaders(w, rid, session, project)
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)

	var pending []byte
	buf := make([]byte, 32*1024)
	flushOut := func(b []byte) bool {
		if len(b) == 0 {
			return true
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	for {
		n, er := resp.Body.Read(buf)
		if n > 0 {
			chunk := bytes.ReplaceAll(buf[:n], []byte("\r\n"), []byte("\n"))
			pending = append(pending, chunk...)
			// Carve out complete frames; keep the tail buffered.
			for {
				idx := bytes.Index(pending, []byte("\n\n"))
				if idx < 0 {
					break
				}
				frame := pending[:idx]
				pending = pending[idx+2:]
				data := frameData(frame)
				if data == "" || data == "[DONE]" {
					continue
				}
				var ev map[string]any
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					continue
				}
				for _, ue := range parseOneEvent(native, ev) {
					if out := tc.push(ue); !flushOut(out) {
						return
					}
				}
			}
		}
		if er != nil {
			break
		}
	}
	// Upstream ended: emit any held terminal event. A stream that ended
	// without one gets a synthesized stop so the client never hangs.
	if !tc.finished && tc.pendingDone == nil {
		tc.push(uevent{kind: "done", stop: "stop"})
	}
	if out := tc.flush(); out != nil {
		flushOut(out)
	}
	log.Printf("%s %s -> %d %s->%s live %dms sess=%s", r.Method, upPath, resp.StatusCode,
		native, client, time.Since(start).Milliseconds(), shortID(session))
}

// parseOneEvent parses a single already-decoded SSE data object.
func parseOneEvent(from Protocol, ev map[string]any) []uevent {
	switch from {
	case ProtoResponses:
		return parseResponsesEvent(ev)
	case ProtoMessages:
		return parseMessagesEvent(ev)
	default:
		return parseChatEvent(ev)
	}
}

func setEchoHeaders(w http.ResponseWriter, rid, session, project string) {
	if rid != "" {
		w.Header().Set("x-request-id", rid)
	}
	if session != "" {
		w.Header().Set("x-muse-session", session)
	}
	if project != "" {
		w.Header().Set("x-muse-project", project)
	}
}

// roundTrip executes the egress strategy: direct first (unless banned),
// then the pool with fresh connections per attempt.
func (s *Server) roundTrip(ctx context.Context, r *http.Request, upPath string, body []byte, extra map[string]string) (*http.Response, error) {
	build := func() (*http.Request, error) {
		var reader *bytes.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		} else {
			reader = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(ctx, r.Method, s.cfg.Upstream+upPath, reader)
		if err != nil {
			return nil, err
		}
		copyHeaderStrip(req.Header, r.Header)
		req.Header.Set("Authorization", "Bearer "+s.cfg.ZenKey)
		for k, v := range extra {
			if req.Header.Get(k) == "" || strings.HasPrefix(strings.ToLower(k), "x-opencode-") || k == "Accept" || k == "User-Agent" {
				req.Header.Set(k, v)
			}
		}
		if len(body) > 0 {
			req.ContentLength = int64(len(body))
		}
		return req, nil
	}

	if !s.cfg.PreferDirect {
		t := s.legacy[int(s.rr.Add(1))%len(s.legacy)]
		req, err := build()
		if err != nil {
			return nil, err
		}
		return t.client.Do(req)
	}

	if time.Now().UnixNano() >= s.banUntil.Load() && s.direct != nil {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := s.direct.client.Do(req)
		if err == nil && (resp.StatusCode/100 == 2 || deterministic4xx(resp.StatusCode)) {
			return resp, nil
		}
		if err != nil || shouldFailover(resp.StatusCode, nil) {
			s.banDirect(resp, err)
		}
		if err == nil && len(s.pools) == 0 {
			return resp, nil
		}
		closeResp(resp)
		if err != nil && len(s.pools) == 0 {
			return nil, err
		}
	}

	attempts := s.cfg.PoolMaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts && len(s.pools) > 0; i++ {
		last := i == attempts-1
		t := s.pickPool()
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := t.client.Do(req)
		if err == nil && (resp.StatusCode/100 == 2 || deterministic4xx(resp.StatusCode)) {
			return resp, nil
		}
		if !last {
			// Another fresh dial follows; this body is useless.
			closeResp(resp)
			lastErr = err
			continue
		}
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
	return nil, lastErr
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(r.Header.Get(n)); v != "" {
			return v
		}
	}
	return ""
}

func copyHeaderStrip(dst, src http.Header) {
	for k, vv := range src {
		if hopHeaders[strings.ToLower(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// streamCopy relays a body, flushing after every chunk so SSE stays live.
func streamCopy(w http.ResponseWriter, rc io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, er := rc.Read(buf)
		if n > 0 {
			if _, ew := w.Write(buf[:n]); ew != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if er != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
