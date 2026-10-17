package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// proxyPool is a dynamic, health-verified egress pool. Sources are
// subscription URLs; every fetch is followed by a concurrent health
// check, and only proxies that pass join the pool. Entries are replaced
// wholesale on each refresh so dead nodes never accumulate.
//
// The pool is hot-swappable: servers own a *proxyPool pointer and always
// read the current generation atomically, so a refresh never blocks
// in-flight requests.
type proxyPool struct {
	mu sync.RWMutex
	// items is the flattened rotation list. bySource keeps each
	// subscription's live nodes separately so a refresh of one source
	// never drops the nodes another source contributed; order preserves
	// first-seen source order for a stable flattening.
	items    []*proxyTransport
	bySource map[string][]*proxyTransport
	dead     map[string]int
	order    []string
	// cursor walks items so consecutive requests spread across nodes.
	cursor atomic.Uint64
	// stats for /healthz
	lastFetch   atomic.Int64
	lastChecked atomic.Int64
	sourceCount atomic.Int64
	liveCount   atomic.Int64
	deadCount   atomic.Int64
}

func newProxyPool() *proxyPool {
	return &proxyPool{bySource: map[string][]*proxyTransport{}, dead: map[string]int{}}
}

func (p *proxyPool) snapshot() []*proxyTransport {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.items
}

func (p *proxyPool) len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.items)
}

// next returns the next live proxy, round-robin. nil when the pool is empty.
func (p *proxyPool) next() *proxyTransport {
	items := p.snapshot()
	if len(items) == 0 {
		return nil
	}
	i := int(p.cursor.Add(1)-1) % len(items)
	return items[i]
}

// replace installs a whole pool at once (static inline proxies and tests).
// Subscription sources must use replaceSource so they do not evict each
// other.
func (p *proxyPool) replace(items []*proxyTransport) {
	p.mu.Lock()
	p.bySource = map[string][]*proxyTransport{}
	p.dead = map[string]int{}
	p.order = nil
	p.items = items
	p.mu.Unlock()
	p.liveCount.Store(int64(len(items)))
	p.deadCount.Store(0)
}

// replaceSource installs one subscription's live nodes and rebuilds the
// rotation list from every source. dead is this source's rejected count
// for the round, summed across sources for /healthz.
func (p *proxyPool) replaceSource(key string, items []*proxyTransport, dead int) {
	p.mu.Lock()
	if p.bySource == nil {
		p.bySource = map[string][]*proxyTransport{}
	}
	if p.dead == nil {
		p.dead = map[string]int{}
	}
	if _, seen := p.bySource[key]; !seen {
		p.order = append(p.order, key)
	}
	p.bySource[key] = items
	p.dead[key] = dead
	all := make([]*proxyTransport, 0, len(p.items)+len(items))
	deadTotal := 0
	for _, k := range p.order {
		all = append(all, p.bySource[k]...)
		deadTotal += p.dead[k]
	}
	p.items = all
	p.mu.Unlock()
	p.liveCount.Store(int64(len(all)))
	p.deadCount.Store(int64(deadTotal))
}

// proxyStats feeds the health endpoint.
type proxyStats struct {
	Sources     int   `json:"sources"`
	Live        int   `json:"live"`
	Rejected    int   `json:"rejected"`
	LastFetch   int64 `json:"last_fetch"`
	LastChecked int64 `json:"last_checked"`
}

func (p *proxyPool) stats() proxyStats {
	if p == nil {
		return proxyStats{}
	}
	return proxyStats{
		Sources:     int(p.sourceCount.Load()),
		Live:        len(p.snapshot()),
		Rejected:    int(p.deadCount.Load()),
		LastFetch:   p.lastFetch.Load(),
		LastChecked: p.lastChecked.Load(),
	}
}

// startProxyPools is retained for tests; production startup uses
// waitProxySources so the listener opens with a verified pool.
func startProxyPools(ctx context.Context, cfg *Config, pool *proxyPool) {
	if pool == nil || len(cfg.ProxySources) == 0 {
		return
	}
	pool.sourceCount.Store(int64(len(cfg.ProxySources)))
	for i := range cfg.ProxySources {
		src := cfg.ProxySources[i]
		go func() {
			runProxySourceOnce(ctx, src, pool, nil)
			if src.RefreshHours > 0 {
				runProxySourceLoop(ctx, src, pool, nil)
			}
		}()
	}
}

// fetchProxyList downloads and parses one subscription.
func fetchProxyList(ctx context.Context, src ProxySource) ([]string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "text/plain, */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return parseProxyLines(string(body), src.Protocol, src.Country), nil
}

// parseProxyLines accepts any mix of http(s)://, socks5(h):// URLs,
// host:port pairs, host:port:label lines and IP:PORT:USER:PASS (plain
// HTTP proxy) lines. Comments and blanks are ignored; results are
// deduplicated in order. When country is non-empty only lines carrying a
// matching trailing label are kept, so a geo-labelled list can be
// narrowed without a lookup service.
func parseProxyLines(body, defaultProtocol, country string) []string {
	if defaultProtocol == "" {
		defaultProtocol = "http"
	}
	want := strings.ToLower(strings.TrimSpace(country))
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") ||
			strings.HasPrefix(line, ";") || strings.HasPrefix(line, "//") {
			continue
		}
		host, port := "", ""
		rest := ""
		label := ""
		if strings.Contains(line, "://") {
			if want != "" {
				continue // no label to verify
			}
			u, err := url.Parse(strings.Fields(line)[0])
			if err != nil || u.Host == "" {
				continue
			}
			if !validProxyProtocols[strings.ToLower(u.Scheme)] {
				continue
			}
			host, port = u.Hostname(), u.Port()
			if u.User != nil {
				rest = u.User.String() + "@"
			}
			p := strings.ToLower(u.Scheme) + "://" + rest + net.JoinHostPort(host, port)
			if port != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
			continue
		}
		// Scheme-less shapes:
		//   host:port
		//   host:port:label     (e.g. hideip.me appends a country name)
		//   ip:port:user:pass
		parts := strings.Split(line, ":")
		switch len(parts) {
		case 2:
			host, port = parts[0], parts[1]
		case 3:
			// A trailing non-numeric field is a label (country name),
			// not a credential. A numeric third field matches no known
			// shape, so the line is rejected rather than guessed at.
			if _, err := strconv.Atoi(parts[2]); err == nil {
				continue
			}
			host, port, label = parts[0], parts[1], parts[2]
		case 4:
			host, port = parts[0], parts[1]
			rest = parts[2] + ":" + parts[3] + "@"
		default:
			continue
		}
		if want != "" {
			if label == "" || !strings.Contains(strings.ToLower(label), want) {
				continue
			}
		}
		if host == "" || port == "" {
			continue
		}
		p := defaultProtocol + "://" + rest + net.JoinHostPort(host, port)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// gateProbeConfig is what an inference probe needs: the Responses
// endpoint, one free model id and the credential to send.
type gateProbeConfig struct {
	URL   string
	Model string
	Key   string
}

// responsesURL derives the Responses endpoint from a models test URL.
func responsesURL(testURL string) string {
	u := strings.TrimSuffix(strings.TrimSpace(testURL), "/")
	for _, suffix := range []string{"/v1/models", "/models"} {
		if strings.HasSuffix(u, suffix) {
			return strings.TrimSuffix(u, suffix) + strings.Replace(suffix, "models", "responses", 1)
		}
	}
	return u
}

// gateProbe sends one minimal genuine-shaped inference request through tr.
// The free-tier gate rejects most public proxy egress IPs with 403
// FreeTierError even when the node reaches the API, so a reachability
// check alone admits nodes that can never serve a completion. Only a 2xx
// proves this egress is usable.
func gateProbe(ctx context.Context, tr *http.Transport, cfg *gateProbeConfig, timeout time.Duration) error {
	if cfg == nil || cfg.Model == "" || cfg.URL == "" {
		return fmt.Errorf("gate probe not configured")
	}
	session := FreshSessionID()
	body := map[string]any{
		"model":             cfg.Model,
		"stream":            true,
		"instructions":      "You are OpenCode, a coding agent. Reply with the single word ok.",
		"input":             []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with the single word ok"}}}},
		"max_output_tokens": 16,
		"store":             false,
		"prompt_cache_key":  session,
		"include":           []any{"reasoning.encrypted_content"},
	}
	ensureTools(body, ProtoResponses)
	raw, err := marshalNoEscape(body)
	if err != nil {
		return err
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", cfg.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	key := cfg.Key
	if key == "" {
		key = "public"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Authorization", "Bearer "+key)
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id", "X-Session-Id"} {
		req.Header.Set(name, session)
	}
	req.Header.Set("x-opencode-project", CanonicalProjectID("session:"+session))
	req.Header.Set("x-opencode-client", defaultClient)
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusForbidden:
		// The gate's definitive verdict on this egress IP.
		return fmt.Errorf("gate rejected egress (403): %s", truncateText(string(head), 120))
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("gate probe rate limited (429): %s", truncateText(string(head), 120))
	default:
		// Anything else is inconclusive (transient upstream or model
		// churn), so reachability already proved this node is worth
		// keeping rather than discarding it on our own probe's failure.
		return nil
	}
}

func truncateText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// checkProxyList probes every candidate concurrently and returns the live
// ones sorted by latency, plus tested/rejected counts. Rejected entries
// are logged at debug level only (a fresh list typically has many dead).
func checkProxyList(ctx context.Context, candidates []string, src ProxySource, gate *gateProbeConfig) (live []*proxyTransport, tested, dead int) {
	type result struct {
		transport *proxyTransport
		latency   time.Duration
	}
	work := make(chan string)
	results := make(chan result, len(candidates))
	timeout := time.Duration(src.TimeoutSeconds) * time.Second
	gateTimeout := timeout * 2
	if gateTimeout < 15*time.Second {
		gateTimeout = 15 * time.Second
	}

	var wg sync.WaitGroup
	workers := src.Concurrency
	if workers > len(candidates) {
		workers = len(candidates)
	}
	if workers < 1 {
		workers = 1
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range work {
				tr, err := newForwardTransport(raw, src.TestURL)
				if err != nil {
					results <- result{}
					continue
				}
				start := time.Now()
				if err := probeThrough(ctx, tr, src.TestURL, timeout); err != nil {
					closeIdle(tr)
					results <- result{}
					continue
				}
				if src.Probe == "inference" && gate != nil {
					g := *gate
					if g.URL == "" {
						g.URL = responsesURL(src.TestURL)
					}
					if err := gateProbe(ctx, tr, &g, gateTimeout); err != nil {
						closeIdle(tr)
						results <- result{}
						continue
					}
				}
				results <- result{
					transport: &proxyTransport{
						name:   raw,
						client: &http.Client{Transport: tr},
					},
					latency: time.Since(start),
				}
			}
		}()
	}
	go func() {
		for _, c := range candidates {
			select {
			case work <- c:
			case <-ctx.Done():
				close(work)
				return
			}
		}
		close(work)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		tested++
		if r.transport == nil {
			dead++
			continue
		}
		live = append(live, r.transport)
	}
	sort.SliceStable(live, func(i, j int) bool {
		return live[i].latency < live[j].latency
	})
	return live, tested, dead
}

// probeThrough performs one health check through a proxy transport.
// Any HTTP response proves the route works; only transport errors or
// 5xx/429 mean the node is unusable for us.
func probeThrough(ctx context.Context, tr *http.Transport, testURL string, timeout time.Duration) error {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", testURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", defaultUA)
	client := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func closeIdle(tr *http.Transport) {
	if tr != nil {
		tr.CloseIdleConnections()
	}
}

// redactProxySource keeps subscription tokens out of logs.
func redactProxySource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "source"
	}
	return u.Host + u.Path
}
