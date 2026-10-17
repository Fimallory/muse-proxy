package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config is the full service configuration. Unknown fields are rejected.
type Config struct {
	Listen          string   `json:"listen"`
	ServerKeys      []string `json:"server_keys"`
	ZenKey          string   `json:"zen_key"`
	Upstream        string   `json:"upstream"`
	Proxies         []string `json:"proxies"`
	ReuseProxyConns bool     `json:"reuse_proxy_connections"`

	// PreferDirect tries the local egress IP first and only fails over
	// to the proxy pool when direct is unavailable (rate limit, 5xx or
	// transport failure). Pool attempts always dial fresh connections.
	PreferDirect          bool `json:"prefer_direct"`
	DirectCooldownSeconds int  `json:"direct_cooldown_seconds"`
	PoolMaxAttempts       int  `json:"pool_max_attempts"`

	HashStorePath   string  `json:"hash_store_path"`
	HashTTLDays     float64 `json:"hash_ttl_days"`
	HashMaxEntries  int     `json:"hash_max_entries"`
	HashPrefixChars int     `json:"hash_prefix_chars"`

	CatalogURL          string  `json:"catalog_url"`
	CatalogPath         string  `json:"catalog_path"`
	CatalogRefreshHours float64 `json:"catalog_refresh_hours"`

	// FreeOnly exposes only free models: /v1/models hides paid models and
	// drops the "-free" suffix from the free ones, and requests for a
	// known paid model are refused. Stripped names map back to the real
	// upstream id (so a client can call "muse-spark-1.3-contributor").
	FreeOnly bool `json:"free_only"`

	// Proxy lists are subscription URLs returning one proxy per line.
	// Every URL is fetched at startup, its proxies are health-checked
	// concurrently, and only live ones join the pool. See ProxySource.
	ProxySources []ProxySource `json:"proxy_sources"`

	// RetryEmpty drops empty replies instead of passing them through:
	// when an upstream 2xx carries no text and no tool calls, the
	// request is re-sent internally (fresh connection) up to
	// MaxEmptyRetries extra times. The last reply is forwarded even
	// if still empty.
	RetryEmpty      bool `json:"retry_empty"`
	MaxEmptyRetries int  `json:"max_empty_retries"`

	// EmptyGuardTimeoutSeconds bounds how long a streaming reply may be
	// held back while the empty-reply guard decides whether it carries
	// content. On expiry the held prefix is flushed and the stream goes
	// live, so a slow first token can never stall the client. 0 waits
	// until content arrives or the stream ends. Default 30.
	EmptyGuardTimeoutSeconds int `json:"empty_guard_timeout_seconds"`

	RequestTimeoutSeconds int `json:"request_timeout_seconds"`

	HashTTL        time.Duration `json:"-"`
	RequestTimeout time.Duration `json:"-"`
	DirectCooldown time.Duration `json:"-"`
	CatalogRefresh time.Duration `json:"-"`
	EmptyGuardHold time.Duration `json:"-"`
}

// ProxySource is one subscription endpoint plus its health-check policy.
type ProxySource struct {
	URL string `json:"url"`
	// Protocol overrides scheme detection: "http" | "https" | "socks5".
	// Empty means derive from each line (lines without a scheme default
	// to ProtocolDefault, itself "http" when unset).
	Protocol string `json:"protocol"`
	// Country keeps only lines carrying a matching trailing label, e.g.
	// "1.2.3.4:1080:United States" with Country "United States". Lists
	// without labels cannot be verified, so their lines are dropped when
	// this is set; use a country-filtered endpoint instead for those.
	Country string `json:"country"`
	// Probe selects the health check. "models" (default) only proves the
	// node can reach the API. "inference" also sends one minimal
	// completion, because the free-tier gate rejects most public proxy
	// egress IPs with 403 FreeTierError even when they are reachable —
	// reachability alone does not make a node usable.
	Probe string `json:"probe"`
	// TestURL is fetched through each proxy to prove it works.
	// Defaults to https://opencode.ai/zen/v1/models.
	TestURL string `json:"test_url"`
	// TimeoutSeconds bounds one health check. Default 8.
	TimeoutSeconds int `json:"timeout_seconds"`
	// Concurrency caps parallel checks. Default 24.
	Concurrency int `json:"concurrency"`
	// MaxKeep caps how many live proxies this source contributes.
	// 0 means unlimited.
	MaxKeep int `json:"max_keep"`
	// RefreshHours re-fetches and re-checks this source. 0 disables
	// periodic refresh (startup check only).
	RefreshHours float64 `json:"refresh_hours"`
}

// v2.0.29 moved the provider API from /zen/v1 to /inference/v1. The old
// /zen/v1 path is now the legacy migration route and enforces a strict
// free-tier gate (403 FreeTierError for anything it does not recognise),
// while /inference/v1 is what a genuine v2 client calls.
const defaultUpstream = "https://opencode.ai/inference"

const defaultProxyTestURL = "https://opencode.ai/inference/v1/models"

var validProxyProtocols = map[string]bool{
	"http": true, "https": true, "socks5": true, "socks5h": true,
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := Config{
		Listen:                   "127.0.0.1:3334",
		Upstream:                 defaultUpstream,
		Proxies:                  []string{"direct"},
		PreferDirect:             true,
		DirectCooldownSeconds:    120,
		PoolMaxAttempts:          3,
		HashStorePath:            "./hashes.json",
		HashTTLDays:              3,
		HashMaxEntries:           50000,
		HashPrefixChars:          10000,
		CatalogURL:               "https://models.opencode.ai/api.json",
		CatalogPath:              "./catalog.json",
		CatalogRefreshHours:      24,
		RetryEmpty:               true,
		MaxEmptyRetries:          2,
		EmptyGuardTimeoutSeconds: 30,
		RequestTimeoutSeconds:    600,
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if v := strings.TrimSpace(os.Getenv("LISTEN")); v != "" {
		cfg.Listen = v
	}
	trimList(&cfg.ServerKeys)
	cfg.ZenKey = strings.TrimSpace(cfg.ZenKey)
	if cfg.ZenKey == "" {
		cfg.ZenKey = "public"
	}
	cfg.Upstream = strings.TrimRight(strings.TrimSpace(cfg.Upstream), "/")
	// Auto-migrate the legacy /zen base path to /inference (v2.0.29).
	// /zen/v1 is now the migration route with a strict free-tier gate;
	// /inference/v1 is the endpoint a genuine v2 client uses.
	if u, err := url.Parse(cfg.Upstream); err == nil && u.Host == "opencode.ai" {
		switch strings.TrimRight(u.Path, "/") {
		case "/zen":
			u.Path = "/inference"
			log.Printf("upstream %q is the legacy /zen path; using %q instead (opencode v2.0.29 moved the provider API)", cfg.Upstream, strings.TrimRight(u.String(), "/"))
			cfg.Upstream = strings.TrimRight(u.String(), "/")
		case "/zen/go":
			u.Path = "/inference/go"
			log.Printf("upstream %q is the legacy /zen/go path; using %q instead (opencode v2.0.29 moved the provider API)", cfg.Upstream, strings.TrimRight(u.String(), "/"))
			cfg.Upstream = strings.TrimRight(u.String(), "/")
		}
	}
	for i := range cfg.Proxies {
		cfg.Proxies[i] = strings.TrimSpace(cfg.Proxies[i])
	}

	if cfg.Listen == "" {
		return nil, fmt.Errorf("listen must not be empty")
	}
	if len(cfg.ServerKeys) == 0 {
		return nil, fmt.Errorf("server_keys must contain at least one local key")
	}
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("upstream must be an http or https URL")
	}
	if len(cfg.Proxies) == 0 {
		return nil, fmt.Errorf("proxies must contain at least one entry")
	}
	for _, raw := range cfg.Proxies {
		if raw == "direct" {
			continue
		}
		pu, err := url.Parse(raw)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL")
		}
		switch strings.ToLower(pu.Scheme) {
		case "http", "https", "socks5", "socks5h":
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", pu.Scheme)
		}
	}
	if cfg.HashTTLDays <= 0 {
		return nil, fmt.Errorf("hash_ttl_days must be positive")
	}
	if cfg.HashMaxEntries < 100 {
		return nil, fmt.Errorf("hash_max_entries must be at least 100")
	}
	if cfg.HashPrefixChars < 100 {
		return nil, fmt.Errorf("hash_prefix_chars must be at least 100")
	}
	if cfg.RequestTimeoutSeconds < 1 {
		return nil, fmt.Errorf("request_timeout_seconds must be at least 1")
	}
	if cfg.CatalogRefreshHours <= 0 {
		return nil, fmt.Errorf("catalog_refresh_hours must be positive")
	}
	if cfg.CatalogURL == "" || cfg.CatalogPath == "" {
		return nil, fmt.Errorf("catalog_url and catalog_path must not be empty")
	}
	if cfg.DirectCooldownSeconds < 0 {
		return nil, fmt.Errorf("direct_cooldown_seconds must not be negative")
	}
	if cfg.PoolMaxAttempts < 1 {
		return nil, fmt.Errorf("pool_max_attempts must be at least 1")
	}
	if cfg.MaxEmptyRetries < 0 {
		return nil, fmt.Errorf("max_empty_retries must not be negative")
	}
	if cfg.EmptyGuardTimeoutSeconds < 0 {
		return nil, fmt.Errorf("empty_guard_timeout_seconds must not be negative")
	}
	for i := range cfg.ProxySources {
		src := &cfg.ProxySources[i]
		src.URL = strings.TrimSpace(src.URL)
		if src.URL == "" {
			return nil, fmt.Errorf("proxy_sources[%d].url must not be empty", i)
		}
		su, err := url.Parse(src.URL)
		if err != nil || su.Host == "" || (su.Scheme != "http" && su.Scheme != "https") {
			return nil, fmt.Errorf("proxy_sources[%d].url must be an http or https URL", i)
		}
		src.Protocol = strings.ToLower(strings.TrimSpace(src.Protocol))
		if src.Protocol != "" && !validProxyProtocols[src.Protocol] {
			return nil, fmt.Errorf("proxy_sources[%d].protocol must be http, https, socks5 or socks5h", i)
		}
		src.Probe = strings.ToLower(strings.TrimSpace(src.Probe))
		if src.Probe != "" && src.Probe != "models" && src.Probe != "inference" {
			return nil, fmt.Errorf("proxy_sources[%d].probe must be models or inference", i)
		}
		src.TestURL = strings.TrimSpace(src.TestURL)
		if src.TestURL == "" {
			src.TestURL = defaultProxyTestURL
		}
		tu, err := url.Parse(src.TestURL)
		if err != nil || tu.Host == "" || (tu.Scheme != "http" && tu.Scheme != "https") {
			return nil, fmt.Errorf("proxy_sources[%d].test_url must be an http or https URL", i)
		}
		if src.TimeoutSeconds == 0 {
			src.TimeoutSeconds = 8
		}
		if src.TimeoutSeconds < 1 {
			return nil, fmt.Errorf("proxy_sources[%d].timeout_seconds must be positive", i)
		}
		if src.Concurrency == 0 {
			src.Concurrency = 24
		}
		if src.Concurrency < 1 {
			return nil, fmt.Errorf("proxy_sources[%d].concurrency must be positive", i)
		}
		if src.MaxKeep < 0 {
			return nil, fmt.Errorf("proxy_sources[%d].max_keep must not be negative", i)
		}
		if src.RefreshHours < 0 {
			return nil, fmt.Errorf("proxy_sources[%d].refresh_hours must not be negative", i)
		}
	}
	cfg.HashTTL = time.Duration(cfg.HashTTLDays * 24 * float64(time.Hour))
	cfg.RequestTimeout = time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	cfg.DirectCooldown = time.Duration(cfg.DirectCooldownSeconds) * time.Second
	cfg.CatalogRefresh = time.Duration(cfg.CatalogRefreshHours * float64(time.Hour))
	cfg.EmptyGuardHold = time.Duration(cfg.EmptyGuardTimeoutSeconds) * time.Second
	return &cfg, nil
}

func trimList(in *[]string) {
	out := (*in)[:0]
	for _, v := range *in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	*in = out
}
