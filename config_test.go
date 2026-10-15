package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseConfig(upstream string) string {
	return `{
		"listen": "127.0.0.1:3334",
		"server_keys": ["k"],
		"zen_key": "public",
		"upstream": "` + upstream + `",
		"proxies": ["direct"]
	}`
}

// TestUpstreamMigration covers the v2.0.29 move from /zen to /inference:
// the legacy base path must be rewritten so existing configs keep working.
func TestUpstreamMigration(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://opencode.ai/zen", "https://opencode.ai/inference"},
		{"https://opencode.ai/zen/", "https://opencode.ai/inference"},
		{"https://opencode.ai/zen/go", "https://opencode.ai/inference/go"},
		{"https://opencode.ai/inference", "https://opencode.ai/inference"},
		{"https://example.test/zen", "https://example.test/zen"},
	}
	for _, c := range cases {
		cfg, err := loadConfig(writeConfig(t, baseConfig(c.in)))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if cfg.Upstream != c.want {
			t.Fatalf("%s: got %q, want %q", c.in, cfg.Upstream, c.want)
		}
	}
}

// TestDefaultUpstreamIsInference guards the v2 default: a config that
// omits `upstream` must land on the endpoint genuine v2 clients use.
func TestDefaultUpstreamIsInference(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{
		"listen": "127.0.0.1:3334",
		"server_keys": ["k"],
		"zen_key": "public"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "https://opencode.ai/inference" {
		t.Fatalf("default upstream = %q", cfg.Upstream)
	}
	if defaultProxyTestURL != "https://opencode.ai/inference/v1/models" {
		t.Fatalf("default proxy test url = %q", defaultProxyTestURL)
	}
}

// TestZenKeyDefaultsToPublic guards the keyless default: omitting
// `zen_key` must yield the anonymous free-tier credential, not an error.
func TestZenKeyDefaultsToPublic(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{
		"listen": "127.0.0.1:3334",
		"server_keys": ["k"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ZenKey != "public" {
		t.Fatalf("default zen_key = %q, want public", cfg.ZenKey)
	}
}
