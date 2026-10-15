package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestExposedModelName(t *testing.T) {
	cases := map[string]string{
		"muse-spark-1.3-contributor-free": "muse-spark-1.3-contributor",
		"mimo-v2.6-flash-free":            "mimo-v2.6-flash",
		"big-pickle":                      "big-pickle",
		"gpt-5":                           "gpt-5",
	}
	for in, want := range cases {
		if got := exposedModelName(in); got != want {
			t.Fatalf("exposedModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCostIsFree(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`{"input":0,"output":0,"cache_read":0}`, true},
		{`{"input":0}`, true},
		{`{"input":1.25,"output":4.25}`, false},
		{`{"output":3}`, true}, // no input price -> free
		{`[{"input":0},{"input":0}]`, true},
		{`[{"input":0},{"input":2}]`, false},
		{`null`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := costIsFree(json.RawMessage(c.in)); got != c.want {
			t.Fatalf("costIsFree(%s) = %v, want %v", c.in, got, c.want)
		}
	}
}

func testCatalog(t *testing.T) *catalog {
	t.Helper()
	c := openCatalog(filepath.Join(t.TempDir(), "c.json"), "http://example.invalid", time.Hour)
	c.table = map[string]Protocol{
		"muse-spark-1.3-contributor-free": ProtoResponses,
		"mimo-v2.6-flash-free":            ProtoChat,
		"big-pickle":                      ProtoChat,
		"gpt-5":                           ProtoResponses,
		"minimax-m2.5":                    ProtoChat,
		"minimax-m2.5-free":               ProtoMessages,
	}
	c.free = map[string]bool{
		"muse-spark-1.3-contributor-free": true,
		"mimo-v2.6-flash-free":            true,
		"big-pickle":                      true,
		"minimax-m2.5-free":               true,
	}
	c.alias = map[string]string{
		"muse-spark-1.3-contributor": "muse-spark-1.3-contributor-free",
		"mimo-v2.6-flash":            "mimo-v2.6-flash-free",
		"minimax-m2.5":               "minimax-m2.5-free",
	}
	return c
}

// TestDeprecatedModelsDropped mirrors the OpenCode client: retired models
// never enter the table, the free set or the alias map.
func TestDeprecatedModelsDropped(t *testing.T) {
	var raw catalogDirectory
	if err := json.Unmarshal([]byte(`{"opencode":{
		"npm":"@ai-sdk/openai-compatible",
		"models":{
			"mimo-v2.5-free":{"provider":{"npm":"@ai-sdk/openai-compatible"},"cost":{"input":0},"status":"deprecated"},
			"exo-free":{"cost":{"input":0},"status":"Deprecated"},
			"mimo-v2.6-flash-free":{"cost":{"input":0}},
			"gpt-5":{"provider":{"npm":"@ai-sdk/openai"},"cost":{"input":1}}
		}}}`), &raw); err != nil {
		t.Fatal(err)
	}
	table, free, alias := buildCatalogTable(raw)
	if _, ok := table["mimo-v2.5-free"]; ok {
		t.Fatal("deprecated model must not be routed")
	}
	if _, ok := table["exo-free"]; ok {
		t.Fatal("deprecated model must not be routed (case-insensitive)")
	}
	if free["mimo-v2.5-free"] || free["exo-free"] {
		t.Fatal("deprecated models must not be free")
	}
	if _, ok := alias["mimo-v2.5"]; ok {
		t.Fatal("deprecated models must not gain an alias")
	}
	if table["mimo-v2.6-flash-free"] != ProtoChat || !free["mimo-v2.6-flash-free"] {
		t.Fatal("active free model lost")
	}
	if table["gpt-5"] != ProtoResponses || free["gpt-5"] {
		t.Fatal("paid model mishandled")
	}
}

// TestFreeAliasWinsOverPaidModel: the stripped name of a free model must// resolve to the free model even when a paid model shares that id (the
// two can differ in protocol, e.g. minimax-m2.5 chat vs -free messages).
func TestFreeAliasWinsOverPaidModel(t *testing.T) {
	c := testCatalog(t)
	real, ok := c.aliasOf("minimax-m2.5")
	if !ok || real != "minimax-m2.5-free" {
		t.Fatalf("aliasOf(minimax-m2.5) = %q,%v", real, ok)
	}
	if p := c.nativeProtocol(real); p != ProtoMessages {
		t.Fatalf("free alias protocol = %q, want messages", p)
	}
	if !c.isFree(real) {
		t.Fatal("aliased model must be free")
	}
	if c.isFree("minimax-m2.5") {
		t.Fatal("paid model must not be free")
	}
}

func TestFilterFreeModels(t *testing.T) {
	s := &Server{cfg: &Config{FreeOnly: true}, catalog: testCatalog(t)}
	if !s.freeFilterActive() {
		t.Fatal("filter should be active with free data present")
	}
	raw := []byte(`{"object":"list","data":[
		{"id":"gpt-5","object":"model","owned_by":"opencode"},
		{"id":"muse-spark-1.3-contributor-free","object":"model","owned_by":"opencode"},
		{"id":"mimo-v2.6-flash-free","object":"model","owned_by":"opencode"},
		{"id":"big-pickle","object":"model","owned_by":"opencode"},
		{"id":"minimax-m2.5","object":"model","owned_by":"opencode"}
	]}`)
	var doc map[string]any
	if err := json.Unmarshal(s.filterFreeModels(raw), &doc); err != nil {
		t.Fatal(err)
	}
	items := doc["data"].([]any)
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	want := map[string]bool{
		"muse-spark-1.3-contributor": true,
		"mimo-v2.6-flash":            true,
		"big-pickle":                 true,
		// minimax-m2.5-free is absent upstream but known free: appended.
		"minimax-m2.5": true,
	}
	for _, id := range ids {
		if !want[id] {
			t.Fatalf("unexpected id in free list: %q (all: %v)", id, ids)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing ids: %v (got %v)", want, ids)
	}
	for _, id := range ids {
		if id == "gpt-5" {
			t.Fatal("paid model leaked into free list")
		}
	}
}

// TestFreeFilterInactiveWithoutData guards the fail-open rule: an empty
// free set must not hide every model.
func TestFreeFilterInactiveWithoutData(t *testing.T) {
	s := &Server{cfg: &Config{FreeOnly: true}, catalog: openCatalog(filepath.Join(t.TempDir(), "c.json"), "http://example.invalid", time.Hour)}
	if s.freeFilterActive() {
		t.Fatal("filter must stay off until free data is known")
	}
	s2 := &Server{cfg: &Config{FreeOnly: false}, catalog: testCatalog(t)}
	if s2.freeFilterActive() {
		t.Fatal("filter must stay off when free_only is false")
	}
}
