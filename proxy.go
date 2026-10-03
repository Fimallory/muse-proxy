package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	name    string
	client  *http.Client
	latency time.Duration
}

// newForwardTransport builds an egress transport for one proxy URL.
// testURL is only used to pick sane timeouts; it is not contacted here.
func newForwardTransport(raw, testURL string) (*http.Transport, error) {
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
		return tr, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("parse proxy %q: %w", raw, err)
	}
	tr.Proxy = http.ProxyURL(u)
	// A rotating pool assigns an egress node per connection, so a pooled
	// keep-alive tunnel would pin every request to one node. HTTP/2
	// multiplexes too, so it goes as well, and ALPN is pinned to
	// http/1.1 or the server answers with h2 frames on an h1 connection
	// (malformed response).
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
	return tr, nil
}

// Server is the forwarding gateway.
type Server struct {
	cfg      *Config
	store    *hashStore
	catalog  *catalog
	pool     *proxyPool
	direct   *proxyTransport
	rr       atomic.Uint64
	banUntil atomic.Int64 // direct egress skipped while now < banUntil
}

func newServer(cfg *Config, store *hashStore, cat *catalog, pool *proxyPool) (*Server, error) {
	s := &Server{cfg: cfg, store: store, catalog: cat, pool: pool}
	if cfg.PreferDirect {
		tr, err := newForwardTransport("direct", "")
		if err != nil {
			return nil, err
		}
		s.direct = &proxyTransport{name: "direct", client: &http.Client{Transport: tr}}
	}
	// Static inline proxies (reuse_proxy_connections) become their own
	// tiny pool unless proxy_sources already supply a dynamic one.
	if pool == nil {
		pool = newProxyPool()
	}
	if pool.len() == 0 {
		var static []*proxyTransport
		for _, raw := range cfg.Proxies {
			if raw == "direct" {
				continue
			}
			tr, err := newForwardTransport(raw, "")
			if err != nil {
				return nil, err
			}
			if cfg.ReuseProxyConns {
				tr.DisableKeepAlives = false
				tr.ForceAttemptHTTP2 = true
				tr.TLSNextProto = nil
				tr.MaxIdleConns = 64
				tr.MaxIdleConnsPerHost = 64
				tlsCfg := tr.TLSClientConfig
				if tlsCfg != nil {
					tlsCfg = tlsCfg.Clone()
					tlsCfg.NextProtos = nil
					tr.TLSClientConfig = tlsCfg
				}
			}
			static = append(static, &proxyTransport{name: raw, client: &http.Client{Transport: tr}})
		}
		if len(static) > 0 {
			pool.replace(static)
		}
	}
	s.pool = pool
	return s, nil
}

func (s *Server) close() {}

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
	}
	if st := s.pool.stats(); st.Sources > 0 || st.Live > 0 {
		egress["pool"] = st
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
// differ. Non-streaming (collapse) clients get the stream folded into a
// single JSON object; streaming clients get a live relay with the
// empty-reply guard applied.
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

	if collapse {
		// A stream:false client cannot tell liveness from events, so the
		// answer is buffered, verified non-empty, folded to JSON.
		res, err := s.fetchVerified(ctx, r, upPath, body, extra, native, model)
		if err != nil {
			log.Printf("upstream %s %s error: %v", r.Method, upPath, err)
			writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "upstream request failed"}})
			return
		}
		if res.status/100 != 2 {
			w.Header().Set("Content-Type", "application/json")
			setEchoHeaders(w, rid, session, project)
			w.WriteHeader(res.status)
			_, _ = w.Write(res.body)
			log.Printf("%s %s -> %d %s%dms sess=%s", r.Method, upPath,
				res.status, conv, time.Since(start).Milliseconds(), shortID(session))
			return
		}
		var out []byte
		var status int
		if native == client && native == ProtoResponses {
			// Verbatim completed object: highest fidelity.
			out, status = collapseSSE(res.body, model, "")
		} else {
			events := parseSSEEvents(native, res.body)
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

	s.forwardStreaming(ctx, w, r, upPath, body, extra, native, client, model, start, conv, rid, session, project)
}

// forwardStreaming relays a streaming reply live, retrying an empty one
// internally on a fresh connection. The empty-reply guard holds the
// response prefix only until it is proven non-empty (or the hold
// deadline passes), so the client keeps seeing first events promptly.
func (s *Server) forwardStreaming(ctx context.Context, w http.ResponseWriter, r *http.Request, upPath string, body []byte, extra map[string]string, native, client Protocol, model string, start time.Time, conv, rid, session, project string) {
	tries := 1
	if s.cfg.RetryEmpty {
		tries += s.cfg.MaxEmptyRetries
	}
	var lastErr error
	for i := 0; i < tries; i++ {
		force := i == tries-1
		resp, err := s.roundTrip(ctx, r, upPath, body, extra)
		if err != nil {
			lastErr = err
			if force {
				break
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			// Errors are never retried: relay them verbatim.
			copyHeaderStrip(w.Header(), resp.Header)
			w.Header().Del("Content-Length")
			setEchoHeaders(w, rid, session, project)
			w.WriteHeader(resp.StatusCode)
			streamCopy(w, resp.Body)
			resp.Body.Close()
			log.Printf("%s %s -> %d %s%dms sess=%s", r.Method, upPath, resp.StatusCode,
				conv, time.Since(start).Milliseconds(), shortID(session))
			return
		}
		committed, empty, serr := s.streamGuarded(w, resp, native, client, model, upPath,
			start, rid, session, project, force)
		resp.Body.Close()
		if committed {
			note := ""
			if empty {
				note = "empty "
			}
			log.Printf("%s %s -> %d %s%s%dms sess=%s", r.Method, upPath, resp.StatusCode,
				conv, note, time.Since(start).Milliseconds(), shortID(session))
			return
		}
		if serr != nil {
			lastErr = serr
		}
		if force {
			break
		}
		if empty {
			log.Printf("empty %s reply for %s, retrying internally (%d/%d)", native, model, i+1, tries)
		} else {
			log.Printf("stream %s %s attempt %d/%d failed: %v", r.Method, upPath, i+1, tries, serr)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		log.Printf("upstream %s %s error: %v", r.Method, upPath, lastErr)
	}
	writeJSON(w, 502, map[string]any{"error": map[string]any{"message": "upstream request failed"}})
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

// setEchoHeaders writes the correlation headers every response carries.
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

	if s.pool.len() == 0 && s.direct == nil {
		return nil, fmt.Errorf("no egress route configured (no proxies live, direct disabled)")
	}

	// Pool-only mode: no direct route to prefer.
	if !s.cfg.PreferDirect {
		attempts := s.cfg.PoolMaxAttempts
		if attempts < 1 {
			attempts = 1
		}
		var lastErr error
		for i := 0; i < attempts; i++ {
			t := s.pool.next()
			if t == nil {
				break
			}
			req, err := build()
			if err != nil {
				return nil, err
			}
			resp, err := t.client.Do(req)
			if err == nil && (resp.StatusCode/100 == 2 || deterministic4xx(resp.StatusCode)) {
				return resp, nil
			}
			if i < attempts-1 {
				closeResp(resp)
				lastErr = err
				continue
			}
			if err != nil {
				return nil, err
			}
			return resp, nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("proxy pool is empty")
		}
		return nil, lastErr
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
		if err == nil && s.pool.len() == 0 {
			return resp, nil
		}
		closeResp(resp)
		if err != nil && s.pool.len() == 0 {
			return nil, err
		}
	}

	attempts := s.cfg.PoolMaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts && s.pool.len() > 0; i++ {
		last := i == attempts-1
		t := s.pool.next()
		if t == nil {
			break
		}
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
	if lastErr == nil {
		lastErr = fmt.Errorf("all egress routes failed")
	}
	return nil, lastErr
}

// waitProxySources runs the first fetch+check round for all sources and
// blocks until each has either installed a pool or failed. The listener
// opens afterwards so the first request sees a verified pool.
func waitProxySources(ctx context.Context, cfg *Config, pool *proxyPool, maxWait time.Duration) {
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		pool.sourceCount.Store(int64(len(cfg.ProxySources)))
		for i := range cfg.ProxySources {
			src := cfg.ProxySources[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				runProxySourceOnce(ctx, src, pool)
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(maxWait):
		log.Printf("proxy check still running after %s; starting listener anyway", maxWait)
	}
	// Periodic refresh for sources that asked for it.
	for i := range cfg.ProxySources {
		src := cfg.ProxySources[i]
		if src.RefreshHours <= 0 {
			continue
		}
		go runProxySourceLoop(ctx, src, pool)
	}
}

// runProxySourceOnce performs one fetch + check + install round.
func runProxySourceOnce(ctx context.Context, src ProxySource, pool *proxyPool) {
	start := time.Now()
	raw, err := fetchProxyList(ctx, src)
	if err != nil {
		log.Printf("proxy source %s: fetch failed: %v", redactProxySource(src.URL), err)
		return
	}
	pool.lastFetch.Store(time.Now().Unix())
	if len(raw) == 0 {
		log.Printf("proxy source %s: empty list, keeping previous pool", redactProxySource(src.URL))
		return
	}
	live, tested, dead := checkProxyList(ctx, raw, src)
	pool.lastChecked.Store(time.Now().Unix())
	pool.deadCount.Store(int64(dead))
	if len(live) == 0 {
		log.Printf("proxy source %s: %d checked, none alive", redactProxySource(src.URL), tested)
		return
	}
	if src.MaxKeep > 0 && len(live) > src.MaxKeep {
		// Probes are sorted by latency; keep the fastest MaxKeep nodes.
		live = live[:src.MaxKeep]
	}
	pool.replace(live)
	log.Printf("proxy source %s: %d checked -> %d live, %d rejected in %s",
		redactProxySource(src.URL), tested, len(live), dead, time.Since(start).Round(time.Millisecond))
}

// runProxySourceLoop re-runs the fetch+check round on schedule.
func runProxySourceLoop(ctx context.Context, src ProxySource, pool *proxyPool) {
	ticker := time.NewTicker(time.Duration(src.RefreshHours * float64(time.Hour)))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runProxySourceOnce(ctx, src, pool)
		}
	}
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
