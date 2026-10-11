package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// chatToolSSE mimics a real chat-completions tool-call stream: the first
// tool_calls chunk carries id+name with empty arguments, later chunks
// carry only argument fragments, then a terminal finish_reason.
func chatToolSSE() string {
	return `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"shell","arguments":""},"index":0}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":"{\"command\""},"index":0}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"function":{"arguments":": \"echo hi\"}"},"index":0}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n"
}

func transcodeAll(t *testing.T, from Protocol, sse string, to Protocol) string {
	t.Helper()
	var out strings.Builder
	tc := newTranscoder(to, "m")
	for _, ev := range parseSSEEvents(from, []byte(sse)) {
		out.Write(tc.push(ev))
	}
	out.Write(tc.flush())
	return out.String()
}

// eventsOf decodes the emitted SSE into (event-name, data-object) pairs.
func eventsOf(t *testing.T, sse string) [][2]any {
	t.Helper()
	var out [][2]any
	for _, frame := range splitSSEFrames([]byte(sse)) {
		name := ""
		for _, line := range strings.Split(string(frame), "\n") {
			if rest, ok := strings.CutPrefix(line, "event:"); ok {
				name = strings.TrimSpace(rest)
			}
		}
		data := frameData(frame)
		if data == "" || data == "[DONE]" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			continue
		}
		out = append(out, [2]any{name, m})
	}
	return out
}

func TestResponsesToolCallIDsStayConsistent(t *testing.T) {
	got := transcodeAll(t, ProtoChat, chatToolSSE(), ProtoResponses)
	evs := eventsOf(t, got)

	var addedID string
	var addedIdx any
	var deltaIDs []string
	var doneID, doneArgs string
	var completedID, completedArgs string
	sawDone := false

	for _, e := range evs {
		name, m := e[0].(string), e[1].(map[string]any)
		switch name {
		case "response.output_item.added":
			item, _ := m["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				addedID, _ = item["id"].(string)
				addedIdx = m["output_index"]
			}
		case "response.function_call_arguments.delta":
			id, _ := m["item_id"].(string)
			deltaIDs = append(deltaIDs, id)
			if m["output_index"] != addedIdx {
				t.Fatalf("delta output_index %v != added %v", m["output_index"], addedIdx)
			}
		case "response.output_item.done":
			item, _ := m["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				sawDone = true
				doneID, _ = item["id"].(string)
				doneArgs, _ = item["arguments"].(string)
			}
		case "response.completed":
			resp, _ := m["response"].(map[string]any)
			for _, o := range marr(resp, "output") {
				om, _ := o.(map[string]any)
				if om != nil && om["type"] == "function_call" {
					completedID, _ = om["id"].(string)
					completedArgs, _ = om["arguments"].(string)
				}
			}
		}
	}

	const wantArgs = `{"command": "echo hi"}`
	if addedID == "" {
		t.Fatal("no function_call output_item.added emitted")
	}
	if len(deltaIDs) == 0 {
		t.Fatal("no function_call_arguments.delta emitted")
	}
	for i, id := range deltaIDs {
		if id != addedID {
			t.Fatalf("delta[%d] item_id %q != item id %q\n%s", i, id, addedID, got)
		}
	}
	if !sawDone {
		t.Fatalf("no response.output_item.done for the function_call\n%s", got)
	}
	if doneID != addedID {
		t.Fatalf("output_item.done id %q != %q", doneID, addedID)
	}
	if doneArgs != wantArgs {
		t.Fatalf("output_item.done arguments %q != %q", doneArgs, wantArgs)
	}
	if completedID != addedID {
		t.Fatalf("completed output id %q != %q", completedID, addedID)
	}
	if completedArgs != wantArgs {
		t.Fatalf("completed arguments %q != %q", completedArgs, wantArgs)
	}
}

func TestResponsesMessageIDsStayConsistent(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","content":"he"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	got := transcodeAll(t, ProtoChat, sse, ProtoResponses)
	evs := eventsOf(t, got)

	var msgID string
	var deltaIDs []string
	for _, e := range evs {
		name, m := e[0].(string), e[1].(map[string]any)
		switch name {
		case "response.output_item.added":
			item, _ := m["item"].(map[string]any)
			if item != nil && item["type"] == "message" {
				msgID, _ = item["id"].(string)
			}
		case "response.content_part.added":
			if id, _ := m["item_id"].(string); id != msgID {
				t.Fatalf("content_part.added item_id %q != %q", id, msgID)
			}
		case "response.output_text.delta":
			id, _ := m["item_id"].(string)
			deltaIDs = append(deltaIDs, id)
		}
	}
	if msgID == "" || len(deltaIDs) == 0 {
		t.Fatalf("missing message events\n%s", got)
	}
	for i, id := range deltaIDs {
		if id != msgID {
			t.Fatalf("text delta[%d] item_id %q != %q", i, id, msgID)
		}
	}
}
