package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
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
	mu    sync.RWMutex
	items []*proxyTransport
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
	return &proxyPool{}
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

func (p *proxyPool) replace(items []*proxyTransport) {
	p.mu.Lock()
	p.items = items
	p.mu.Unlock()
	p.liveCount.Store(int64(len(items)))
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
			runProxySourceOnce(ctx, src, pool)
			if src.RefreshHours > 0 {
				runProxySourceLoop(ctx, src, pool)
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
	return parseProxyLines(string(body), src.Protocol), nil
}

// parseProxyLines accepts any mix of http(s)://, socks5(h):// URLs,
// host:port pairs, and IP:PORT:USER:PASS (plain HTTP proxy) lines.
// Comments and blanks are ignored; results are deduplicated in order.
func parseProxyLines(body, defaultProtocol string) []string {
	if defaultProtocol == "" {
		defaultProtocol = "http"
	}
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
		if strings.Contains(line, "://") {
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
		// Scheme-less: host:port or ip:port:user:pass
		parts := strings.Split(line, ":")
		switch len(parts) {
		case 2:
			host, port = parts[0], parts[1]
		case 4:
			host, port = parts[0], parts[1]
			rest = parts[2] + ":" + parts[3] + "@"
		default:
			continue
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

// checkProxyList probes every candidate concurrently and returns the live
// ones sorted by latency, plus tested/rejected counts. Rejected entries
// are logged at debug level only (a fresh list typically has many dead).
func checkProxyList(ctx context.Context, candidates []string, src ProxySource) (live []*proxyTransport, tested, dead int) {
	type result struct {
		transport *proxyTransport
		latency   time.Duration
	}
	work := make(chan string)
	results := make(chan result, len(candidates))
	timeout := time.Duration(src.TimeoutSeconds) * time.Second

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
