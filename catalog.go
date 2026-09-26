package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Protocol is a wire protocol spoken on a Zen endpoint.
type Protocol string

const (
	ProtoChat      Protocol = "chat"
	ProtoResponses Protocol = "responses"
	ProtoMessages  Protocol = "messages"
	ProtoGoogle    Protocol = "google"
)

// protocolOfNPM maps a provider SDK name to its wire protocol, mirroring
// how the OpenCode client picks an SDK in BUNDLED_PROVIDERS.
func protocolOfNPM(npm string) Protocol {
	l := strings.ToLower(npm)
	if strings.Contains(l, "anthropic") {
		return ProtoMessages
	}
	if npm == "@ai-sdk/openai" || strings.HasSuffix(npm, "/openai") {
		return ProtoResponses
	}
	if strings.Contains(l, "openai-compatible") {
		return ProtoChat
	}
	if strings.Contains(l, "google") {
		return ProtoGoogle
	}
	return ""
}

// Path returns the Zen endpoint path suffix for a protocol.
func (p Protocol) Path() string {
	switch p {
	case ProtoChat:
		return "/v1/chat/completions"
	case ProtoResponses:
		return "/v1/responses"
	case ProtoMessages:
		return "/v1/messages"
	default:
		return ""
	}
}

// catalog resolves model -> native protocol from the public capability
// directory (the same file the OpenCode client fetches at startup):
// model.provider.npm ?? provider.npm, then npm -> protocol.
type catalog struct {
	mu      sync.RWMutex
	table   map[string]Protocol
	updated time.Time
	source  string // "live" | "disk" | "none"
	path    string
	url     string
	refresh time.Duration
	stop    chan struct{}
	wg      sync.WaitGroup
}

type catalogDisk struct {
	Updated int64             `json:"updated"`
	Table   map[string]string `json:"table"`
}

func openCatalog(path, url string, refresh time.Duration) *catalog {
	c := &catalog{
		table:   make(map[string]Protocol),
		source:  "none",
		path:    path,
		url:     url,
		refresh: refresh,
		stop:    make(chan struct{}),
	}
	if data, err := os.ReadFile(path); err == nil {
		var disk catalogDisk
		if err := json.Unmarshal(data, &disk); err == nil && len(disk.Table) > 0 {
			for m, p := range disk.Table {
				if pp := Protocol(p); pp == ProtoChat || pp == ProtoResponses || pp == ProtoMessages || pp == ProtoGoogle {
					c.table[m] = pp
				}
			}
			c.updated = time.Unix(disk.Updated, 0)
			c.source = "disk"
			log.Printf("catalog loaded %d models from disk", len(c.table))
		}
	} else if !os.IsNotExist(err) {
		log.Printf("catalog disk load: %v", err)
	}
	return c
}

// resolve returns the native protocol and whether the model is known.
// Unknown models fall back to chat (the provider default), mirroring the
// client's provider-level npm.
func (c *catalog) resolve(model string) (Protocol, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.table[model]
	return p, ok
}

func (c *catalog) snapshot() (entries int, updated int64, source string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.table), c.updated.Unix(), c.source
}

func (c *catalog) start(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.refreshNow()
		ticker := time.NewTicker(c.refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stop:
				return
			case <-ticker.C:
				c.refreshNow()
			}
		}
	}()
}

func (c *catalog) close() {
	close(c.stop)
	c.wg.Wait()
}

// refreshNow fetches the directory and swaps the table. Any single source
// failing keeps the previous table: never go empty on a bad refresh.
func (c *catalog) refreshNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.url, nil)
	if err != nil {
		log.Printf("catalog refresh: %v", err)
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("catalog refresh: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		log.Printf("catalog refresh: HTTP %d", resp.StatusCode)
		return
	}
	// The directory is keyed by provider name at the top level.
	var raw map[string]struct {
		NPM    string `json:"npm"`
		API    string `json:"api"`
		Models map[string]struct {
			Provider *struct {
				NPM string `json:"npm"`
				API string `json:"api"`
			} `json:"provider"`
		} `json:"models"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&raw); err != nil {
		log.Printf("catalog refresh: decode: %v", err)
		return
	}
	table := make(map[string]Protocol)
	for _, provKey := range []string{"opencode", "opencode-go"} {
		prov, ok := raw[provKey]
		if !ok {
			continue
		}
		defProto := protocolOfNPM(prov.NPM)
		for id, m := range prov.Models {
			npm := prov.NPM
			if m.Provider != nil && m.Provider.NPM != "" {
				npm = m.Provider.NPM
			}
			p := protocolOfNPM(npm)
			if p == "" {
				p = defProto
			}
			if p == "" {
				continue
			}
			// First provider wins for shared IDs; prefer explicit overrides.
			if _, dup := table[id]; dup {
				continue
			}
			table[id] = p
		}
	}
	if len(table) == 0 {
		log.Printf("catalog refresh: empty table, keeping previous")
		return
	}
	c.mu.Lock()
	c.table = table
	c.updated = time.Now()
	c.source = "live"
	c.mu.Unlock()
	c.save()
	log.Printf("catalog refreshed: %d models (live)", len(table))
}

func (c *catalog) save() {
	c.mu.RLock()
	disk := catalogDisk{Updated: c.updated.Unix(), Table: make(map[string]string, len(c.table))}
	for m, p := range c.table {
		disk.Table[m] = string(p)
	}
	c.mu.RUnlock()
	data, err := json.Marshal(disk)
	if err != nil {
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	_ = tmp.Chmod(0o600)
	_ = tmp.Sync()
	_ = tmp.Close()
	if err := os.Rename(tmpName, c.path); err != nil {
		os.Remove(tmpName)
		return
	}
}

// nativeProtocol resolves with provider-default fallback to chat.
func (c *catalog) nativeProtocol(model string) Protocol {
	if p, ok := c.resolve(model); ok && p != "" {
		return p
	}
	return ProtoChat
}
