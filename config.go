package main

import (
	"encoding/json"
	"fmt"
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

	RequestTimeoutSeconds int `json:"request_timeout_seconds"`

	HashTTL        time.Duration `json:"-"`
	RequestTimeout time.Duration `json:"-"`
	DirectCooldown time.Duration `json:"-"`
	CatalogRefresh time.Duration `json:"-"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg := Config{
		Listen:                "127.0.0.1:3334",
		Upstream:              "https://opencode.ai/zen",
		Proxies:               []string{"direct"},
		PreferDirect:          true,
		DirectCooldownSeconds: 120,
		PoolMaxAttempts:       3,
		HashStorePath:         "./hashes.json",
		HashTTLDays:           3,
		HashMaxEntries:        50000,
		HashPrefixChars:       10000,
		CatalogURL:            "https://models.opencode.ai/api.json",
		CatalogPath:           "./catalog.json",
		CatalogRefreshHours:   24,
		RequestTimeoutSeconds: 600,
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
	cfg.Upstream = strings.TrimRight(strings.TrimSpace(cfg.Upstream), "/")
	for i := range cfg.Proxies {
		cfg.Proxies[i] = strings.TrimSpace(cfg.Proxies[i])
	}

	if cfg.Listen == "" {
		return nil, fmt.Errorf("listen must not be empty")
	}
	if len(cfg.ServerKeys) == 0 {
		return nil, fmt.Errorf("server_keys must contain at least one local key")
	}
	if cfg.ZenKey == "" {
		return nil, fmt.Errorf("zen_key must not be empty")
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
	cfg.HashTTL = time.Duration(cfg.HashTTLDays * 24 * float64(time.Hour))
	cfg.RequestTimeout = time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	cfg.DirectCooldown = time.Duration(cfg.DirectCooldownSeconds) * time.Second
	cfg.CatalogRefresh = time.Duration(cfg.CatalogRefreshHours * float64(time.Hour))
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
