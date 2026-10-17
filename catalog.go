package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
// model.provider.npm ?? provider.npm, then npm -> protocol. It also
// tracks which models are free (cost.input == 0) and the suffix-stripped
// names a free-only gateway exposes.
type catalog struct {
	mu    sync.RWMutex
	table map[string]Protocol
	// free marks real model ids that carry no charge.
	free map[string]bool
	// alias maps an exposed (suffix-stripped) name back to its real id.
	// Consulted only in free-only mode, where it also wins over table.
	alias   map[string]string
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
	Free    []string          `json:"free,omitempty"`
	Alias   map[string]string `json:"alias,omitempty"`
}

func openCatalog(path, url string, refresh time.Duration) *catalog {
	c := &catalog{
		table:   make(map[string]Protocol),
		free:    make(map[string]bool),
		alias:   make(map[string]string),
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
			for _, id := range disk.Free {
				if _, ok := c.table[id]; ok {
					c.free[id] = true
				}
			}
			for ex, real := range disk.Alias {
				if _, ok := c.table[real]; ok {
					c.alias[ex] = real
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

// exposedModelName is the client-facing name of a free model: the literal
// "-free" suffix is dropped so callers never see it.
func exposedModelName(id string) string {
	return strings.TrimSuffix(id, "-free")
}

// isFree reports whether a real model id carries no charge.
func (c *catalog) isFree(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.free[model]
}

// aliasOf resolves an exposed (suffix-stripped) name back to the real id.
func (c *catalog) aliasOf(model string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	real, ok := c.alias[model]
	return real, ok
}

// hasFreeInfo reports whether the free set is populated. An empty set
// means the filter must be skipped rather than hide every model.
func (c *catalog) hasFreeInfo() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.free) > 0
}

// freeCount is the number of known free models.
func (c *catalog) freeCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.free)
}

// freeModelNames returns the exposed names of every known free model,
// sorted for a stable listing.
func (c *catalog) freeModelNames() []string {
	c.mu.RLock()
	names := make([]string, 0, len(c.free))
	for id := range c.free {
		names = append(names, exposedModelName(id))
	}
	c.mu.RUnlock()
	sort.Strings(names)
	return names
}

// firstFreeModel returns one real free model id, sorted for determinism,
// or "" when the free set is unknown. The egress gate probe uses it so a
// proxy check never spends a paid model.
func (c *catalog) firstFreeModel() string {
	c.mu.RLock()
	names := make([]string, 0, len(c.free))
	for id := range c.free {
		names = append(names, id)
	}
	c.mu.RUnlock()
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
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
	var raw catalogDirectory
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&raw); err != nil {
		log.Printf("catalog refresh: decode: %v", err)
		return
	}
	table, free, alias := buildCatalogTable(raw)
	if len(table) == 0 {
		log.Printf("catalog refresh: empty table, keeping previous")
		return
	}
	c.mu.Lock()
	c.table = table
	c.free = free
	c.alias = alias
	c.updated = time.Now()
	c.source = "live"
	c.mu.Unlock()
	c.save()
	log.Printf("catalog refreshed: %d models, %d free (live)", len(table), len(free))
}

// catalogDirectory mirrors the shape of models.opencode.ai/api.json.
type catalogDirectory map[string]struct {
	NPM    string `json:"npm"`
	API    string `json:"api"`
	Models map[string]struct {
		Provider *struct {
			NPM string `json:"npm"`
			API string `json:"api"`
		} `json:"provider"`
		Cost   json.RawMessage `json:"cost"`
		Status string          `json:"status"`
	} `json:"models"`
}

// buildCatalogTable folds the capability directory into the routing table,
// the free set and the suffix-stripped alias map. Retired models
// (status "deprecated") are dropped outright, mirroring the OpenCode
// client, which never lists them.
func buildCatalogTable(raw catalogDirectory) (table map[string]Protocol, free map[string]bool, alias map[string]string) {
	table = make(map[string]Protocol)
	free = make(map[string]bool)
	alias = make(map[string]string)
	for _, provKey := range []string{"opencode", "opencode-go"} {
		prov, ok := raw[provKey]
		if !ok {
			continue
		}
		defProto := protocolOfNPM(prov.NPM)
		for id, m := range prov.Models {
			if strings.EqualFold(strings.TrimSpace(m.Status), "deprecated") {
				continue
			}
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
			if !costIsFree(m.Cost) {
				continue
			}
			free[id] = true
			// Expose free models without their "-free" suffix. The
			// alias wins over a same-named paid model in free-only mode
			// (e.g. minimax-m2.5-free and minimax-m2.5 differ in protocol).
			if ex := exposedModelName(id); ex != id {
				if _, dup := alias[ex]; !dup {
					alias[ex] = id
				}
			}
		}
	}
	return table, free, alias
}

// costIsFree reports whether a directory cost entry carries no charge.
// The directory encodes cost as an object (an array of tiers is also
// accepted); a model is free when no tier prices input above zero. A
// missing or null cost block is treated as paid, since free cannot be
// confirmed from it.
func costIsFree(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		return !hasPositiveInput(obj)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, e := range arr {
			if hasPositiveInput(e) {
				return false
			}
		}
		return true
	}
	return false
}

func hasPositiveInput(m map[string]any) bool {
	v, ok := m["input"].(float64)
	return ok && v > 0
}

func (c *catalog) save() {
	c.mu.RLock()
	disk := catalogDisk{
		Updated: c.updated.Unix(),
		Table:   make(map[string]string, len(c.table)),
		Free:    make([]string, 0, len(c.free)),
		Alias:   make(map[string]string, len(c.alias)),
	}
	for m, p := range c.table {
		disk.Table[m] = string(p)
	}
	for id := range c.free {
		disk.Free = append(disk.Free, id)
	}
	sort.Strings(disk.Free)
	for ex, real := range c.alias {
		disk.Alias[ex] = real
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
