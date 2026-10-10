package main

import "testing"

const titlePrompt = `You are a title generator. You output ONLY a thread title. Nothing else.

<task>
Generate a brief title that would help the user find this conversation later.
</task>

<rules>
- you MUST use the same language as the user message you are summarizing
- Never use tools
- NEVER respond to questions, just generate a title
</rules>`

func TestIsTitleRequestResponses(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","stream":true}`)
	p["instructions"] = titlePrompt
	if !isTitleRequest(p) {
		t.Fatal("real title prompt must be detected")
	}
}

func TestIsTitleRequestChatSystemMessage(t *testing.T) {
	p := mustObj(t, `{"model":"m","messages":[
		{"role":"system","content":"You are a title generator. Never use tools."},
		{"role":"user","content":"hi"}]}`)
	if !isTitleRequest(p) {
		t.Fatal("chat system message title prompt must be detected")
	}
}

func TestIsTitleRequestAnthropicSystemBlocks(t *testing.T) {
	p := mustObj(t, `{"model":"m","system":[{"type":"text","text":"You are a title generator."},
		{"type":"text","text":"Never use tools."}],"messages":[]}`)
	if !isTitleRequest(p) {
		t.Fatal("anthropic system blocks must be detected")
	}
}

func TestIsTitleRequestNegative(t *testing.T) {
	cases := []string{
		`{"model":"m","instructions":"You are OpenCode, an AI coding agent. Use the provided tools."}`,
		`{"model":"m","instructions":"You are a title generator."}`,
		`{"model":"m","instructions":"Never use tools."}`,
		`{"model":"m","input":"hi"}`,
	}
	for _, c := range cases {
		if isTitleRequest(mustObj(t, c)) {
			t.Fatalf("false positive on %s", c)
		}
	}
}

func TestApplyToolsDropsForTitleRequest(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi","tools":[{"type":"function","name":"bash",
		"parameters":{"type":"object"}}]}`)
	if !applyTools(p, ProtoResponses, true) {
		t.Fatal("dropping tools must report a change")
	}
	if _, ok := p["tools"]; ok {
		t.Fatalf("tools must be removed, got %v", p["tools"])
	}
	// Idempotent when there is nothing to drop.
	if applyTools(p, ProtoResponses, true) {
		t.Fatal("second drop must be a no-op")
	}
}

func TestApplyToolsNormalizesWhenNotTitle(t *testing.T) {
	p := mustObj(t, `{"model":"m","input":"hi"}`)
	if !applyTools(p, ProtoResponses, false) {
		t.Fatal("missing tools must be synthesized")
	}
	if n := len(p["tools"].([]any)); n != len(coreToolNames) {
		t.Fatalf("want %d tools, got %d", len(coreToolNames), n)
	}
}
