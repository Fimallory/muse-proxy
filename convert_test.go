package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustIR(t *testing.T, proto Protocol, body string) *IRRequest {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return decodeRequest(proto, p)
}

func TestChatResponsesRoundTrip(t *testing.T) {
	chat := `{"model":"m","messages":[
		{"role":"system","content":"Be nice."},
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a\nb"}],
		"tools":[{"type":"function","function":{"name":"bash","description":"Run","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}],
		"tool_choice":"auto","temperature":0.5,"stream":true}`
	ir := mustIR(t, ProtoChat, chat)
	if ir.System != "Be nice." {
		t.Fatalf("system lost: %q", ir.System)
	}
	if len(ir.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d", len(ir.Messages))
	}
	if len(ir.Tools) != 1 || ir.Tools[0].Name != "bash" {
		t.Fatalf("tools lost: %+v", ir.Tools)
	}

	resp := encodeRequest(ProtoResponses, ir)
	in, ok := resp["input"].([]any)
	if !ok || len(in) != 5 { // 3 message items + function_call + function_call_output (sys->instructions)
		t.Fatalf("bad responses input: %v", resp["input"])
	}
	if resp["instructions"] != "Be nice." {
		t.Fatalf("instructions lost: %v", resp["instructions"])
	}
	// Back to chat: must preserve everything material.
	back := decodeRequest(ProtoResponses, resp)
	if back.System != "Be nice." || len(back.Messages) != 5 || len(back.Tools) != 1 {
		t.Fatalf("round trip lossy: sys=%q msgs=%d tools=%d",
			back.System, len(back.Messages), len(back.Tools))
	}
	// Tool call survived with id + args.
	found := false
	for _, m := range back.Messages {
		for _, p := range m.Parts {
			if p.Kind == "tool_call" && p.CallID == "call_1" && p.ToolName == "bash" &&
				strings.Contains(p.Arguments, "ls") {
				found = true
			}
			if p.Kind == "tool_result" && p.ResultFor == "call_1" && p.Text == "a\nb" {
				found = found && true
			}
		}
	}
	if !found {
		t.Fatalf("tool round trip broken: %+v", back.Messages)
	}
}

func TestMessagesAnthropicShapes(t *testing.T) {
	body := `{"model":"m","system":"Sys.","messages":[
		{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
		{"role":"assistant","content":[
			{"type":"text","text":"ok"},
			{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"/x"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"data"}]}],
		"tools":[{"name":"read","description":"R","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto"},"max_tokens":512,"stream":true}`
	ir := mustIR(t, ProtoMessages, body)
	if ir.System != "Sys." || ir.MaxTokens != 512 || len(ir.Tools) != 1 {
		t.Fatalf("decode lossy: %+v", ir)
	}
	// Image data URL reconstructed.
	foundImg := false
	for _, m := range ir.Messages {
		for _, p := range m.Parts {
			if p.Kind == "image" && strings.HasPrefix(p.ImageURL, "data:image/png;base64,AAA") {
				foundImg = true
			}
		}
	}
	if !foundImg {
		t.Fatalf("image lost: %+v", ir.Messages)
	}
	// Encode to chat: tool_use -> tool_calls, tool_result -> tool role.
	chat := encodeRequest(ProtoChat, ir)
	msgs := chat["messages"].([]any)
	var sawCall, sawTool bool
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if m["role"] == "assistant" {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) == 1 {
				fn := tcs[0].(map[string]any)["function"].(map[string]any)
				if fn["name"] == "read" && strings.Contains(fn["arguments"].(string), "/x") {
					sawCall = true
				}
			}
		}
		if m["role"] == "tool" && m["tool_call_id"] == "toolu_1" {
			sawTool = true
		}
	}
	if !sawCall || !sawTool {
		t.Fatalf("anthropic->chat tool mapping broken: %v", msgs)
	}
}

func TestReasoningEffortMapping(t *testing.T) {
	// responses effort -> messages budget
	ir := mustIR(t, ProtoResponses, `{"model":"m","input":"hi","reasoning":{"effort":"high"}}`)
	if ir.ReasoningEffort != "high" {
		t.Fatalf("effort lost: %q", ir.ReasoningEffort)
	}
	msg := encodeRequest(ProtoMessages, ir)
	th := mobj(msg, "thinking")
	if th == nil {
		t.Fatalf("no thinking block: %v", msg)
	}
	var budget int
	switch b := th["budget_tokens"].(type) {
	case float64:
		budget = int(b)
	case int:
		budget = b
	default:
		t.Fatalf("bad budget type %T: %v", th["budget_tokens"], th)
	}
	if budget != 8192 {
		t.Fatalf("want budget 8192, got %v", th)
	}
	if numVal(msg["max_tokens"]) != 8192+4096 {
		t.Fatalf("max_tokens must exceed budget, got %v", msg["max_tokens"])
	}
	// budget -> effort
	ir2 := mustIR(t, ProtoMessages, `{"model":"m","messages":[],"thinking":{"type":"enabled","budget_tokens":16384}}`)
	if ir2.ReasoningEffort != "xhigh" {
		t.Fatalf("want xhigh, got %q", ir2.ReasoningEffort)
	}
	// explicit effort beats budget
	ir3 := mustIR(t, ProtoMessages, `{"model":"m","messages":[],"output_config":{"effort":"max"},"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if ir3.ReasoningEffort != "max" {
		t.Fatalf("explicit effort must win, got %q", ir3.ReasoningEffort)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	ir := &IRRequest{Model: "m", ToolChoice: "required"}
	if mobj(encodeRequest(ProtoMessages, ir), "tool_choice")["type"] != "any" {
		t.Fatal("required -> any")
	}
	ir2 := mustIR(t, ProtoMessages, `{"model":"m","messages":[],"tool_choice":{"type":"any"}}`)
	if ir2.ToolChoice != "required" {
		t.Fatalf("any -> required, got %q", ir2.ToolChoice)
	}
	ir3 := mustIR(t, ProtoChat, `{"model":"m","messages":[],"tool_choice":{"type":"function","function":{"name":"bash"}}}`)
	if ir3.ToolChoice != "named" || ir3.NamedTool != "bash" {
		t.Fatalf("named choice lost: %+v", ir3)
	}
	// none drops anthropic tools
	ir4 := &IRRequest{Model: "m", ToolChoice: "none",
		Tools: []IRTool{{Name: "bash", Schema: defaultSchema()}}}
	if _, has := encodeRequest(ProtoMessages, ir4)["tools"]; has {
		t.Fatal("none must drop anthropic tools")
	}
}

func TestSameRoleMerge(t *testing.T) {
	ir := &IRRequest{Model: "m", Messages: []IRMessage{
		{Role: "user", Parts: []IRPart{{Kind: "text", Text: "a"}}},
		{Role: "user", Parts: []IRPart{{Kind: "text", Text: "b"}}},
	}}
	out := encodeRequest(ProtoMessages, ir)
	msgs := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("consecutive user messages must merge, got %d", len(msgs))
	}
}

func TestResponseRoundTrip(t *testing.T) {
	// responses -> chat -> responses preserves text, tools, usage
	src := `{"id":"resp_1","object":"response","created_at":1700000000,"model":"m",
		"status":"completed",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"bash","arguments":"{}"}],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`
	var p map[string]any
	if err := json.Unmarshal([]byte(src), &p); err != nil {
		t.Fatal(err)
	}
	ir := decodeResponse(ProtoResponses, p, "m")
	if ir.Text != "hi" || len(ir.Tools) != 1 || ir.Stop != "tool_calls" {
		t.Fatalf("decode lossy: %+v", ir)
	}
	if ir.Usage.Input != 10 || ir.Usage.Output != 5 {
		t.Fatalf("usage lost: %+v", ir.Usage)
	}
	chat := encodeResponse(ProtoChat, ir)
	ch := msgs0(chat)
	if mstr(ch, "message", "content") != "hi" {
		t.Fatalf("text lost: %v", chat)
	}
	back := decodeResponse(ProtoChat, chat, "m")
	if back.Text != "hi" || len(back.Tools) != 1 || back.Stop != "tool_calls" {
		t.Fatalf("round trip lossy: %+v", back)
	}
}

func numVal(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return -1
	}
}

func msgs0(chat map[string]any) map[string]any {
	choices := marr(chat, "choices")
	if len(choices) == 0 {
		return nil
	}
	m, _ := choices[0].(map[string]any)
	return m
}

func TestStreamTranscodeResponsesToChat(t *testing.T) {
	sse := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_9","model":"m"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","item":{"id":"fc_9","type":"function_call","call_id":"call_9","name":"bash"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"call_9","delta":"{}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_9","status":"completed","output":[{"type":"function_call","id":"fc_9","call_id":"call_9","name":"bash","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":4}}}` + "\n\n"
	events := parseSSEEvents(ProtoResponses, []byte(sse))
	tc := newTranscoder(ProtoChat, "m")
	var out strings.Builder
	for _, ev := range events {
		out.Write(tc.push(ev))
	}
	out.Write(tc.flush())
	s := out.String()
	for _, want := range []string{"hello", `"name":"bash"`, `"finish_reason":"tool_calls"`, "[DONE]",
		`"prompt_tokens":3`, `"completion_tokens":4`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestStreamTranscodeChatToResponses(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	events := parseSSEEvents(ProtoChat, []byte(sse))
	tc := newTranscoder(ProtoResponses, "m")
	var out strings.Builder
	for _, ev := range events {
		out.Write(tc.push(ev))
	}
	out.Write(tc.flush())
	s := out.String()
	for _, want := range []string{"response.created", "response.output_text.delta", `"delta":"hi"`,
		"response.completed", `"input_tokens":7`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	// The terminal completed object must carry the streamed text.
	idx := strings.LastIndex(s, "event: response.completed")
	if idx < 0 {
		t.Fatalf("no completed event in:\n%s", s)
	}
	tail := s[idx:]
	if !strings.Contains(tail, `"text":"hi"`) {
		t.Fatalf("completed object lost accumulated text:\n%s", tail)
	}
}

func TestEventsToChatJSON(t *testing.T) {
	events := []uevent{
		{kind: "created", createdID: "resp_z", createdModel: "m"},
		{kind: "text", text: "done"},
		{kind: "done", stop: "stop"},
		{kind: "usage", inTokens: 5, outTokens: 6},
	}
	out := eventsToResponseJSON(ProtoChat, events, "m")
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("not json: %s", out)
	}
	if mstr(p, "choices", "") != "" {
		t.Fatalf("choices should be an array: %s", out)
	}
	ch := msgs0(p)
	if ch == nil || mstr(ch, "message", "content") != "done" {
		t.Fatalf("text lost: %s", out)
	}
	u := mobj(p, "usage")
	pi, _ := u["prompt_tokens"].(float64)
	if int(pi) != 5 {
		t.Fatalf("usage lost: %s", out)
	}
}

func TestCatalogResolve(t *testing.T) {
	c := openCatalog(t.TempDir()+"/c.json", "http://example.invalid", 0)
	// Seed table directly (network fetch covered by refresh test below).
	c.table = map[string]Protocol{"a": ProtoResponses, "b": ProtoMessages}
	if p, ok := c.resolve("a"); !ok || p != ProtoResponses {
		t.Fatal("resolve a")
	}
	if _, ok := c.resolve("zzz"); ok {
		t.Fatal("unknown must miss")
	}
	if c.nativeProtocol("zzz") != ProtoChat {
		t.Fatal("unknown defaults to chat")
	}
}

func TestServerWiresCatalog(t *testing.T) {
	c := openCatalog(t.TempDir()+"/c.json", "http://example.invalid", 0)
	c.table = map[string]Protocol{"m": ProtoMessages}
	cfg := &Config{Listen: "127.0.0.1:0", ServerKeys: []string{"k"}, ZenKey: "z",
		Upstream: "http://example.invalid", Proxies: []string{"direct"},
		PreferDirect: true, DirectCooldownSeconds: 0, PoolMaxAttempts: 1,
		HashTTLDays: 3, HashMaxEntries: 100, HashPrefixChars: 100,
		RequestTimeoutSeconds: 30}
	cfg.HashTTL = time.Hour
	cfg.RequestTimeout = 30 * time.Second
	store, err := openStore(t.TempDir()+"/h.json", time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	srv, err := newServer(cfg, store, c)
	if err != nil {
		t.Fatal(err)
	}
	if srv.catalog == nil {
		t.Fatal("server catalog not wired")
	}
	if p := srv.catalog.nativeProtocol("m"); p != ProtoMessages {
		t.Fatalf("want messages, got %q", p)
	}
}

func TestProtocolOfNPM(t *testing.T) {
	cases := map[string]Protocol{
		"@ai-sdk/openai":                ProtoResponses,
		"@ai-sdk/openai-compatible":     ProtoChat,
		"@ai-sdk/anthropic":             ProtoMessages,
		"@ai-sdk/google":                ProtoGoogle,
		"@ai-sdk/amazon-bedrock/mantle": "",
		"weird-sdk":                     "",
	}
	for npm, want := range cases {
		if got := protocolOfNPM(npm); got != want {
			t.Fatalf("%s: want %q got %q", npm, want, got)
		}
	}
}
