package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// transcode.go converts RESPONSES between protocols: non-stream JSON
// conversion plus SSE-to-SSE live transcoding. Same-protocol traffic never
// reaches this file.

// IRResponse is a protocol-neutral inference result.
type IRResponse struct {
	ID        string
	Model     string
	Created   int64
	Text      string
	Reasoning string
	Tools     []IRToolCall
	Stop      string // "stop" | "tool_calls" | "length" | "content_filter" | "error"
	Usage     IRUsage
}

// IRToolCall is one assembled tool invocation.
type IRToolCall struct {
	ID, Name, Arguments string
}

// IRUsage counts tokens in OpenAI semantics (input includes cache reads).
type IRUsage struct {
	Input, Output int
}

// ---------- non-stream: upstream JSON -> IR ----------

func decodeChatResponse(p map[string]any, model string) *IRResponse {
	ir := &IRResponse{Model: model, Stop: "stop"}
	ir.ID = mstr(p, "id")
	if f, ok := mnum(p, "created"); ok {
		ir.Created = int64(f)
	}
	choice, _ := marr(p, "choices")[0].(map[string]any)
	if choice == nil {
		return ir
	}
	msg := mobj(choice, "message")
	ir.Text = mstr(msg, "content")
	for _, tc := range marr(msg, "tool_calls") {
		tm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		ir.Tools = append(ir.Tools, IRToolCall{
			ID: mstr(tm, "id"), Name: mstr(tm, "function", "name"),
			Arguments: mstr(tm, "function", "arguments"),
		})
	}
	switch mstr(choice, "finish_reason") {
	case "tool_calls":
		ir.Stop = "tool_calls"
	case "length":
		ir.Stop = "length"
	case "content_filter":
		ir.Stop = "content_filter"
	case "error", "network_error", "server_error":
		ir.Stop = "error"
	}
	u := mobj(p, "usage")
	if v, ok := mnum(u, "prompt_tokens"); ok {
		ir.Usage.Input = int(v)
	} else if v, ok := mnum(u, "input_tokens"); ok {
		ir.Usage.Input = int(v)
	}
	if v, ok := mnum(u, "completion_tokens"); ok {
		ir.Usage.Output = int(v)
	} else if v, ok := mnum(u, "output_tokens"); ok {
		ir.Usage.Output = int(v)
	}
	return ir
}

func decodeResponsesResponse(p map[string]any, model string) *IRResponse {
	ir := &IRResponse{Model: model, Stop: "stop"}
	ir.ID = mstr(p, "id")
	if v := mstr(p, "model"); v != "" {
		ir.Model = v
	}
	if f, ok := mnum(p, "created_at"); ok {
		ir.Created = int64(f)
	}
	for _, raw := range marr(p, "output") {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			for _, c := range marr(item, "content") {
				pm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := pm["type"].(string); t == "output_text" || t == "text" {
					if s, ok := pm["text"].(string); ok {
						ir.Text += s
					}
				}
			}
		case "function_call":
			id, _ := item["id"].(string)
			callID, _ := item["call_id"].(string)
			if callID == "" {
				callID = id
			}
			args, _ := item["arguments"].(string)
			ir.Tools = append(ir.Tools, IRToolCall{
				ID: callID, Name: mstr(item, "name"), Arguments: args,
			})
		case "reasoning":
			for _, s := range marr(item, "summary") {
				if sm, ok := s.(map[string]any); ok {
					ir.Reasoning += mstr(sm, "text")
				}
			}
		}
	}
	if len(ir.Tools) > 0 {
		ir.Stop = "tool_calls"
	}
	switch mstr(p, "status") {
	case "incomplete":
		ir.Stop = "length"
		if mstr(p, "incomplete_details", "reason") == "content_filter" {
			ir.Stop = "content_filter"
		}
	case "failed":
		ir.Stop = "error"
	}
	u := mobj(p, "usage")
	if v, ok := mnum(u, "input_tokens"); ok {
		ir.Usage.Input = int(v)
	}
	if v, ok := mnum(u, "output_tokens"); ok {
		ir.Usage.Output = int(v)
	}
	return ir
}

func decodeMessagesResponse(p map[string]any, model string) *IRResponse {
	ir := &IRResponse{Model: model, Stop: "stop"}
	ir.ID = mstr(p, "id")
	if v := mstr(p, "model"); v != "" {
		ir.Model = v
	}
	for _, raw := range marr(p, "content") {
		b, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch b["type"] {
		case "text":
			ir.Text += mstr(b, "text")
		case "thinking":
			ir.Reasoning += mstr(b, "thinking")
		case "tool_use":
			var args string
			switch in := b["input"].(type) {
			case string:
				args = in
			default:
				if jb, err := json.Marshal(in); err == nil {
					args = string(jb)
				} else {
					args = "{}"
				}
			}
			ir.Tools = append(ir.Tools, IRToolCall{
				ID: mstr(b, "id"), Name: mstr(b, "name"), Arguments: args,
			})
		}
	}
	switch mstr(p, "stop_reason") {
	case "tool_use":
		ir.Stop = "tool_calls"
	case "max_tokens":
		ir.Stop = "length"
	case "refusal":
		ir.Stop = "content_filter"
	}
	u := mobj(p, "usage")
	if v, ok := mnum(u, "input_tokens"); ok {
		ir.Usage.Input = int(v)
	}
	if v, ok := mnum(u, "output_tokens"); ok {
		ir.Usage.Output = int(v)
	}
	return ir
}

func decodeResponse(proto Protocol, p map[string]any, model string) *IRResponse {
	switch proto {
	case ProtoResponses:
		return decodeResponsesResponse(p, model)
	case ProtoMessages:
		return decodeMessagesResponse(p, model)
	default:
		return decodeChatResponse(p, model)
	}
}

// ---------- non-stream: IR -> client JSON ----------

func chatID(id string) string {
	id = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(id, "chatcmpl-"), "resp_"), "msg_")
	if id == "" {
		return RandomID("chatcmpl", 12)
	}
	return "chatcmpl-" + id
}

func encodeChatResponse(ir *IRResponse) map[string]any {
	msg := map[string]any{"role": "assistant"}
	if ir.Text != "" {
		msg["content"] = ir.Text
	} else {
		msg["content"] = nil
	}
	if ir.Reasoning != "" {
		msg["reasoning_content"] = ir.Reasoning
	}
	finish := "stop"
	switch ir.Stop {
	case "tool_calls":
		finish = "tool_calls"
	case "length":
		finish = "length"
	case "content_filter":
		finish = "content_filter"
	}
	if len(ir.Tools) > 0 {
		finish = "tool_calls"
		calls := make([]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			args := t.Arguments
			if args == "" {
				args = "{}"
			}
			id := t.ID
			if id == "" {
				id = RandomID("call", 12)
			}
			calls = append(calls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": t.Name, "arguments": args},
			})
		}
		msg["tool_calls"] = calls
	}
	created := ir.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	return map[string]any{
		"id": chatID(ir.ID), "object": "chat.completion", "created": created,
		"model": ir.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens": ir.Usage.Input, "completion_tokens": ir.Usage.Output,
			"total_tokens": ir.Usage.Input + ir.Usage.Output,
		},
	}
}

func encodeResponsesResponse(ir *IRResponse) map[string]any {
	status := "completed"
	switch ir.Stop {
	case "length":
		status = "incomplete"
	case "error", "content_filter":
		status = "failed"
	}
	id := ir.ID
	if !strings.HasPrefix(id, "resp_") {
		id = RandomID("resp", 12)
	}
	output := make([]any, 0, len(ir.Tools)+1)
	if ir.Text != "" || len(ir.Tools) == 0 {
		output = append(output, map[string]any{
			"id": RandomID("msg", 12), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": ir.Text, "annotations": []any{}}},
		})
	}
	for _, t := range ir.Tools {
		if t.Name == "" {
			continue
		}
		args := t.Arguments
		if args == "" {
			args = "{}"
		}
		callID := t.ID
		if callID == "" {
			callID = RandomID("call", 12)
		}
		output = append(output, map[string]any{
			"id": RandomID("fc", 12), "type": "function_call", "status": "completed",
			"call_id": callID, "name": t.Name, "arguments": args,
		})
	}
	if ir.Reasoning != "" {
		output = append([]any{map[string]any{
			"id": RandomID("rs", 12), "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": ir.Reasoning}},
		}}, output...)
	}
	created := ir.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": created,
		"model": ir.Model, "status": status, "output": output,
		"usage": map[string]any{
			"input_tokens": ir.Usage.Input, "output_tokens": ir.Usage.Output,
			"total_tokens": ir.Usage.Input + ir.Usage.Output,
		},
	}
}

func encodeMessagesResponse(ir *IRResponse) map[string]any {
	id := ir.ID
	if !strings.HasPrefix(id, "msg_") {
		id = RandomID("msg", 12)
	}
	content := make([]any, 0, len(ir.Tools)+1)
	if ir.Reasoning != "" {
		content = append(content, map[string]any{"type": "text", "text": ir.Reasoning})
	}
	if ir.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": ir.Text})
	}
	stop := "end_turn"
	switch ir.Stop {
	case "tool_calls":
		stop = "tool_use"
	case "length":
		stop = "max_tokens"
	case "content_filter":
		stop = "refusal"
	}
	if len(ir.Tools) > 0 {
		stop = "tool_use"
		for _, t := range ir.Tools {
			if t.Name == "" {
				continue
			}
			var input any = map[string]any{}
			if t.Arguments != "" {
				var v any
				if err := json.Unmarshal([]byte(t.Arguments), &v); err == nil {
					input = v
				} else {
					input = t.Arguments
				}
			}
			tid := t.ID
			if tid == "" {
				tid = RandomID("toolu", 12)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": tid, "name": t.Name, "input": input,
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": ir.Model,
		"content": content, "stop_reason": stop,
		"usage": map[string]any{
			"input_tokens": ir.Usage.Input, "output_tokens": ir.Usage.Output,
		},
	}
}

func encodeResponse(proto Protocol, ir *IRResponse) map[string]any {
	switch proto {
	case ProtoResponses:
		return encodeResponsesResponse(ir)
	case ProtoMessages:
		return encodeMessagesResponse(ir)
	default:
		return encodeChatResponse(ir)
	}
}

// convertJSONResponse converts one non-stream response document.
func convertJSONResponse(from, to Protocol, body []byte, model string) ([]byte, error) {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if m, _ := p["model"].(string); m != "" {
		model = m
	}
	return marshalNoEscape(encodeResponse(to, decodeResponse(from, p, model)))
}

// ---------- streaming: upstream SSE -> semantic events ----------

// uevent is one normalized stream occurrence.
type uevent struct {
	kind string // "text" | "reasoning" | "tool" | "done" | "usage" | "error"
	text string
	// tool:
	toolKey, toolID, toolName, toolArgs string
	toolDone                            bool
	// toolSet means toolArgs carries the complete arguments (a done
	// event), not another fragment to append.
	toolSet bool
	// done:
	stop string
	// usage:
	inTokens, outTokens int
	// error:
	errMsg string
	// created:
	createdID, createdModel string
}

func toolKeyOf(index, id string) string {
	if index != "" {
		return "i:" + index
	}
	return "k:" + id
}

// parseSSEEvents turns raw upstream SSE bytes into semantic events.
func parseSSEEvents(from Protocol, body []byte) []uevent {
	var out []uevent
	for _, frame := range splitSSEFrames(body) {
		data := frameData(frame)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		out = append(out, parseOneEvent(from, ev)...)
	}
	return out
}

// parseOneEvent parses a single already-decoded SSE data object.
func parseOneEvent(from Protocol, ev map[string]any) []uevent {
	switch from {
	case ProtoResponses:
		return parseResponsesEvent(ev)
	case ProtoMessages:
		return parseMessagesEvent(ev)
	default:
		return parseChatEvent(ev)
	}
}

func parseChatEvent(ev map[string]any) []uevent {
	if _, hasErr := ev["error"]; hasErr {
		return []uevent{{kind: "error", errMsg: mstr(ev, "error", "message")}}
	}
	var out []uevent
	if id := mstr(ev, "id"); id != "" {
		out = append(out, uevent{kind: "created", createdID: id, createdModel: mstr(ev, "model")})
	}
	for _, raw := range marr(ev, "choices") {
		ch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delta := mobj(ch, "delta")
		if t := mstr(delta, "content"); t != "" {
			out = append(out, uevent{kind: "text", text: t})
		}
		if t := mstr(delta, "reasoning_content"); t != "" {
			out = append(out, uevent{kind: "reasoning", text: t})
		} else if t := mstr(delta, "reasoning"); t != "" {
			out = append(out, uevent{kind: "reasoning", text: t})
		}
		for _, rtc := range marr(delta, "tool_calls") {
			tm, ok := rtc.(map[string]any)
			if !ok {
				continue
			}
			idx := fmt.Sprint(tm["index"])
			if idx == "<nil>" || idx == "%!s(MISSING)" {
				idx = ""
			}
			id, _ := tm["id"].(string)
			out = append(out, uevent{
				kind: "tool", toolKey: toolKeyOf(idx, id), toolID: id,
				toolName: mstr(tm, "function", "name"), toolArgs: mstr(tm, "function", "arguments"),
			})
		}
		if fr := mstr(ch, "finish_reason"); fr != "" {
			stop := "stop"
			switch fr {
			case "tool_calls":
				stop = "tool_calls"
			case "length":
				stop = "length"
			case "content_filter":
				stop = "content_filter"
			case "error", "network_error", "server_error":
				out = append(out, uevent{kind: "error", errMsg: fr})
				continue
			}
			out = append(out, uevent{kind: "done", stop: stop})
		}
	}
	if u := mobj(ev, "usage"); u != nil {
		in, _ := mnum(u, "prompt_tokens")
		if in == 0 {
			in, _ = mnum(u, "input_tokens")
		}
		ot, _ := mnum(u, "completion_tokens")
		if ot == 0 {
			ot, _ = mnum(u, "output_tokens")
		}
		if in != 0 || ot != 0 {
			out = append(out, uevent{kind: "usage", inTokens: int(in), outTokens: int(ot)})
		}
	}
	return out
}

func parseResponsesEvent(ev map[string]any) []uevent {
	typ, _ := ev["type"].(string)
	switch typ {
	case "response.created":
		return []uevent{{kind: "created",
			createdID: mstr(ev, "response", "id"), createdModel: mstr(ev, "response", "model")}}
	case "response.output_text.delta":
		if t := mstr(ev, "delta"); t != "" {
			return []uevent{{kind: "text", text: t}}
		}
	case "response.reasoning_summary_text.delta":
		if t := mstr(ev, "delta"); t != "" {
			return []uevent{{kind: "reasoning", text: t}}
		}
	case "response.output_item.added":
		item := mobj(ev, "item")
		if mstr(item, "type") == "function_call" {
			id, _ := item["id"].(string)
			return []uevent{{kind: "tool", toolKey: toolKeyOf("", id),
				toolID: id, toolName: mstr(item, "name")}}
		}
	case "response.function_call_arguments.delta":
		return []uevent{{kind: "tool", toolKey: toolKeyOf("", mstr(ev, "item_id")),
			toolArgs: mstr(ev, "delta")}}
	case "response.function_call_arguments.done":
		e := uevent{kind: "tool", toolKey: toolKeyOf("", mstr(ev, "item_id")), toolDone: true}
		if args := mstr(ev, "arguments"); args != "" {
			e.toolArgs = args
			e.toolSet = true
		}
		return []uevent{e}
	case "response.output_text.done", "response.output_item.done", "response.content_part.done":
		return nil
	case "response.completed":
		resp := mobj(ev, "response")
		u := mobj(resp, "usage")
		in, _ := mnum(u, "input_tokens")
		ot, _ := mnum(u, "output_tokens")
		stop := "stop"
		for _, o := range marr(resp, "output") {
			if om, ok := o.(map[string]any); ok && mstr(om, "type") == "function_call" {
				stop = "tool_calls"
			}
		}
		if mstr(resp, "status") == "incomplete" {
			stop = "length"
		} else if mstr(resp, "status") == "failed" {
			return []uevent{
				{kind: "usage", inTokens: int(in), outTokens: int(ot)},
				{kind: "error", errMsg: mstr(resp, "error", "message")},
			}
		}
		out := []uevent{{kind: "done", stop: stop}}
		if in != 0 || ot != 0 {
			out = append([]uevent{{kind: "usage", inTokens: int(in), outTokens: int(ot)}}, out...)
		}
		return out
	case "response.failed":
		return []uevent{{kind: "error", errMsg: mstr(ev, "response", "error", "message")}}
	case "error":
		return []uevent{{kind: "error", errMsg: mstr(ev, "error", "message")}}
	}
	return nil
}

func parseMessagesEvent(ev map[string]any) []uevent {
	typ, _ := ev["type"].(string)
	// Some upstreams put the type on the SSE event line instead of the body.
	switch typ {
	case "message_start":
		msg := mobj(ev, "message")
		u := mobj(msg, "usage")
		in, _ := mnum(u, "input_tokens")
		out := []uevent{{kind: "created", createdID: mstr(msg, "id"), createdModel: mstr(msg, "model")}}
		if in != 0 {
			out = append(out, uevent{kind: "usage", inTokens: int(in)})
		}
		return out
	case "content_block_start":
		idx := fmt.Sprint(ev["index"])
		block := mobj(ev, "content_block")
		switch mstr(block, "type") {
		case "tool_use":
			return []uevent{{kind: "tool", toolKey: toolKeyOf(idx, mstr(block, "id")),
				toolID: mstr(block, "id"), toolName: mstr(block, "name")}}
		case "thinking":
			var out []uevent
			if t := mstr(block, "thinking"); t != "" {
				out = append(out, uevent{kind: "reasoning", text: t})
			}
			return out
		}
	case "content_block_delta":
		idx := fmt.Sprint(ev["index"])
		delta := mobj(ev, "delta")
		switch mstr(delta, "type") {
		case "text_delta":
			if t := mstr(delta, "text"); t != "" {
				return []uevent{{kind: "text", text: t}}
			}
		case "thinking_delta":
			if t := mstr(delta, "thinking"); t != "" {
				return []uevent{{kind: "reasoning", text: t}}
			}
		case "input_json_delta":
			if p, ok := delta["partial_json"].(string); ok && p != "" {
				return []uevent{{kind: "tool", toolKey: "i:" + idx, toolArgs: p}}
			}
		case "signature_delta":
			return nil
		}
	case "content_block_stop":
		return nil
	case "message_delta":
		delta := mobj(ev, "delta")
		u := mobj(ev, "usage")
		ot, _ := mnum(u, "output_tokens")
		stop := "stop"
		switch mstr(delta, "stop_reason") {
		case "tool_use":
			stop = "tool_calls"
		case "max_tokens":
			stop = "length"
		case "refusal":
			stop = "content_filter"
		}
		out := []uevent{{kind: "done", stop: stop}}
		if ot != 0 {
			out = append([]uevent{{kind: "usage", outTokens: int(ot)}}, out...)
		}
		return out
	case "message_stop":
		return []uevent{{kind: "done", stop: "stop"}}
	case "error":
		return []uevent{{kind: "error", errMsg: mstr(ev, "error", "message")}}
	}
	return nil
}

// ---------- streaming: emit client SSE ----------

// transcoder holds cross-event state while re-emitting a foreign stream.
type transcoder struct {
	to    Protocol
	model string

	seq      int
	outIndex int
	sentRole bool

	// chat output
	chatToolIndex map[string]int
	chatNextIndex int

	// responses output
	respMsgOpen  bool
	respToolOpen map[string]bool

	// messages output
	msgStarted bool
	msgIndex   int
	msgText    bool // text block open
	msgReason  bool // thinking block open
	msgTool    string

	createdID string
	finished  bool
	started   bool
	// pendingDone holds a terminal event until usage following it is
	// recorded (upstream terminal sequences are [..., done, usage?] and
	// emitting finish before the usage chunk would drop accounting).
	pendingDone *uevent
	usageIn     int
	usageOut    int
	// acc buffers the full turn so the terminal completed object carries
	// the same content already streamed as deltas.
	accText   strings.Builder
	accReason strings.Builder
	accTools  map[string]*IRToolCall
	accOrder  []string
}

func newTranscoder(to Protocol, model string) *transcoder {
	return &transcoder{
		to: to, model: model,
		chatToolIndex: map[string]int{},
		respToolOpen:  map[string]bool{},
		accTools:      map[string]*IRToolCall{},
	}
}

func (t *transcoder) nextSeq() int {
	t.seq++
	return t.seq
}

func sseFrame(event string, data []byte) []byte {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	return []byte(b.String())
}

func sseData(v any) []byte {
	b, _ := marshalNoEscape(v)
	var out strings.Builder
	out.WriteString("data: ")
	out.Write(b)
	out.WriteString("\n\n")
	return []byte(out.String())
}

// push converts one semantic event into downstream SSE bytes.
func (t *transcoder) push(ev uevent) []byte {
	if t.finished {
		return nil
	}
	switch ev.kind {
	case "created":
		if ev.createdID != "" {
			t.createdID = ev.createdID
		}
		return t.emitStart(ev.createdModel)
	case "text":
		t.accText.WriteString(ev.text)
		return t.emitText(ev.text)
	case "reasoning":
		t.accReason.WriteString(ev.text)
		return t.emitReasoning(ev.text)
	case "tool":
		t.accumulateTool(ev)
		return t.emitTool(ev)
	case "usage":
		t.usageIn += ev.inTokens
		t.usageOut += ev.outTokens
		if t.pendingDone != nil {
			d := t.pendingDone
			t.pendingDone = nil
			t.finished = true
			return t.emitFinish(d.stop)
		}
		return nil
	case "done":
		cp := ev
		if cp.stop == "" {
			cp.stop = "stop"
		}
		t.pendingDone = &cp
		return nil
	case "error":
		var out []byte
		if t.pendingDone != nil {
			d := t.pendingDone
			t.pendingDone = nil
			t.finished = true
			out = t.emitFinish(d.stop)
		}
		t.finished = true
		return append(out, t.emitError(ev.errMsg)...)
	}
	return nil
}

// flush emits a held terminal event. Callers must invoke it when the
// upstream stream ends (and in batch mode after the last event).
func (t *transcoder) flush() []byte {
	if t.pendingDone == nil || t.finished {
		return nil
	}
	d := t.pendingDone
	t.pendingDone = nil
	t.finished = true
	return t.emitFinish(d.stop)
}

// accumulateTool folds a tool event into the completed-object buffer.
func (t *transcoder) accumulateTool(ev uevent) {
	key := ev.toolKey
	if key == "" {
		key = "k:" + ev.toolID
	}
	tc, ok := t.accTools[key]
	if !ok {
		tc = &IRToolCall{ID: ev.toolID, Name: ev.toolName}
		t.accTools[key] = tc
		t.accOrder = append(t.accOrder, key)
	}
	if ev.toolName != "" {
		tc.Name = ev.toolName
	}
	if ev.toolID != "" {
		tc.ID = ev.toolID
	}
	if ev.toolSet {
		tc.Arguments = ev.toolArgs
	} else {
		tc.Arguments += ev.toolArgs
	}
}

func (t *transcoder) modelName(fallback string) string {
	if t.model != "" {
		return t.model
	}
	return fallback
}

func (t *transcoder) emitStart(upstreamModel string) []byte {
	if t.started {
		return nil
	}
	t.started = true
	model := t.modelName(upstreamModel)
	switch t.to {
	case ProtoResponses:
		id := t.createdID
		if !strings.HasPrefix(id, "resp_") {
			id = RandomID("resp", 12)
			t.createdID = id
		}
		var out []byte
		out = append(out, sseFrame("response.created",
			mustJSON(map[string]any{"type": "response.created", "sequence_number": t.nextSeq(),
				"response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model}}))...)
		out = append(out, sseFrame("response.in_progress",
			mustJSON(map[string]any{"type": "response.in_progress", "sequence_number": t.nextSeq(),
				"response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model}}))...)
		return out
	case ProtoMessages:
		id := t.createdID
		if !strings.HasPrefix(id, "msg_") {
			id = RandomID("msg", 12)
			t.createdID = id
		}
		t.msgStarted = true
		return sseFrame("message_start", mustJSON(map[string]any{
			"type": "message_start",
			"message": map[string]any{"id": id, "type": "message", "role": "assistant",
				"model": model, "content": []any{},
				"usage": map[string]any{"input_tokens": t.usageIn, "output_tokens": 0}},
		}))
	default: // chat: no start frame needed
		return nil
	}
}

func (t *transcoder) emitText(text string) []byte {
	if text == "" {
		return nil
	}
	switch t.to {
	case ProtoResponses:
		var out []byte
		if !t.respMsgOpen {
			t.respMsgOpen = true
			idx := t.outIndex
			t.outIndex++
			out = append(out, sseFrame("response.output_item.added",
				mustJSON(map[string]any{"type": "response.output_item.added", "sequence_number": t.nextSeq(),
					"output_index": idx, "item": map[string]any{"id": RandomID("msg", 12),
						"type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}}))...)
			out = append(out, sseFrame("response.content_part.added",
				mustJSON(map[string]any{"type": "response.content_part.added", "sequence_number": t.nextSeq(),
					"item_id": "msg", "output_index": idx, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))...)
		}
		out = append(out, sseFrame("response.output_text.delta",
			mustJSON(map[string]any{"type": "response.output_text.delta", "sequence_number": t.nextSeq(),
				"content_index": 0, "output_index": 0, "delta": text}))...)
		return out
	case ProtoMessages:
		var out []byte
		if !t.msgStarted {
			out = append(out, t.emitStart("")...)
		}
		if t.msgReason {
			out = append(out, sseFrame("content_block_stop",
				mustJSON(map[string]any{"type": "content_block_stop", "index": t.msgIndex}))...)
			t.msgIndex++
			t.msgReason = false
		}
		if t.msgTool != "" {
			out = append(out, sseFrame("content_block_stop",
				mustJSON(map[string]any{"type": "content_block_stop", "index": t.msgIndex}))...)
			t.msgIndex++
			t.msgTool = ""
		}
		if !t.msgText {
			t.msgText = true
			out = append(out, sseFrame("content_block_start",
				mustJSON(map[string]any{"type": "content_block_start", "index": t.msgIndex,
					"content_block": map[string]any{"type": "text", "text": ""}}))...)
		}
		out = append(out, sseFrame("content_block_delta",
			mustJSON(map[string]any{"type": "content_block_delta", "index": t.msgIndex,
				"delta": map[string]any{"type": "text_delta", "text": text}}))...)
		return out
	default: // chat
		var out []byte
		if !t.sentRole {
			t.sentRole = true
			out = append(out, sseData(map[string]any{
				"id": chatID(t.createdID), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": t.modelName(""),
				"choices": []any{map[string]any{"index": 0,
					"delta":         map[string]any{"role": "assistant", "content": ""},
					"finish_reason": nil}},
			})...)
		}
		out = append(out, sseData(map[string]any{
			"id": chatID(t.createdID), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": t.modelName(""),
			"choices": []any{map[string]any{"index": 0,
				"delta":         map[string]any{"content": text},
				"finish_reason": nil}},
		})...)
		return out
	}
}

func (t *transcoder) emitReasoning(text string) []byte {
	if text == "" {
		return nil
	}
	switch t.to {
	case ProtoResponses:
		// Surface reasoning as text; the collapsed path keeps full shape.
		return t.emitText(text)
	case ProtoMessages:
		var out []byte
		if !t.msgStarted {
			out = append(out, t.emitStart("")...)
		}
		if t.msgText || t.msgTool != "" {
			out = append(out, sseFrame("content_block_stop",
				mustJSON(map[string]any{"type": "content_block_stop", "index": t.msgIndex}))...)
			t.msgIndex++
			t.msgText = false
			t.msgTool = ""
		}
		if !t.msgReason {
			t.msgReason = true
			out = append(out, sseFrame("content_block_start",
				mustJSON(map[string]any{"type": "content_block_start", "index": t.msgIndex,
					"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}))...)
		}
		out = append(out, sseFrame("content_block_delta",
			mustJSON(map[string]any{"type": "content_block_delta", "index": t.msgIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": text}}))...)
		return out
	default: // chat extension field, widely accepted
		return sseData(map[string]any{
			"id": chatID(t.createdID), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": t.modelName(""),
			"choices": []any{map[string]any{"index": 0,
				"delta":         map[string]any{"reasoning_content": text},
				"finish_reason": nil}},
		})
	}
}

func (t *transcoder) emitTool(ev uevent) []byte {
	key := ev.toolKey
	if key == "" {
		key = "k:" + ev.toolID
	}
	switch t.to {
	case ProtoResponses:
		var out []byte
		if !t.respToolOpen[key] {
			t.respToolOpen[key] = true
			idx := t.outIndex
			t.outIndex++
			fcid := RandomID("fc", 12)
			out = append(out, sseFrame("response.output_item.added",
				mustJSON(map[string]any{"type": "response.output_item.added", "sequence_number": t.nextSeq(),
					"output_index": idx, "item": map[string]any{"id": fcid, "type": "function_call",
						"status": "in_progress", "arguments": "", "call_id": ev.toolID, "name": ev.toolName}}))...)
		}
		if ev.toolArgs != "" {
			out = append(out, sseFrame("response.function_call_arguments.delta",
				mustJSON(map[string]any{"type": "response.function_call_arguments.delta",
					"sequence_number": t.nextSeq(), "item_id": ev.toolID, "delta": ev.toolArgs}))...)
		}
		if ev.toolDone {
			out = append(out, sseFrame("response.function_call_arguments.done",
				mustJSON(map[string]any{"type": "response.function_call_arguments.done",
					"sequence_number": t.nextSeq(), "item_id": ev.toolID, "arguments": ev.toolArgs}))...)
		}
		return out
	case ProtoMessages:
		var out []byte
		if !t.msgStarted {
			out = append(out, t.emitStart("")...)
		}
		if t.msgText || t.msgReason {
			out = append(out, sseFrame("content_block_stop",
				mustJSON(map[string]any{"type": "content_block_stop", "index": t.msgIndex}))...)
			t.msgIndex++
			t.msgText = false
			t.msgReason = false
		}
		if t.msgTool == "" {
			t.msgTool = key
			callID := ev.toolID
			if callID == "" {
				callID = RandomID("toolu", 12)
			}
			out = append(out, sseFrame("content_block_start",
				mustJSON(map[string]any{"type": "content_block_start", "index": t.msgIndex,
					"content_block": map[string]any{"type": "tool_use", "id": callID,
						"name": ev.toolName, "input": map[string]any{}}}))...)
			_ = callID
		}
		if ev.toolArgs != "" {
			out = append(out, sseFrame("content_block_delta",
				mustJSON(map[string]any{"type": "content_block_delta", "index": t.msgIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.toolArgs}}))...)
		}
		return out
	default: // chat
		var out []byte
		idx, ok := t.chatToolIndex[key]
		if !ok {
			idx = t.chatNextIndex
			t.chatNextIndex++
			t.chatToolIndex[key] = idx
			fn := map[string]any{}
			if ev.toolName != "" {
				fn["name"] = ev.toolName
				fn["arguments"] = ""
			}
			delta := map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": ev.toolID, "type": "function", "function": fn,
			}}}
			out = append(out, sseData(map[string]any{
				"id": chatID(t.createdID), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": t.modelName(""),
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
			})...)
		}
		if ev.toolArgs != "" {
			out = append(out, sseData(map[string]any{
				"id": chatID(t.createdID), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": t.modelName(""),
				"choices": []any{map[string]any{"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": idx, "function": map[string]any{"arguments": ev.toolArgs}}}},
					"finish_reason": nil}},
			})...)
		}
		return out
	}
}

func (t *transcoder) emitFinish(stop string) []byte {
	finish := "stop"
	usageMap := map[string]any{}
	switch t.to {
	case ProtoResponses:
		id := t.createdID
		if !strings.HasPrefix(id, "resp_") {
			id = RandomID("resp", 12)
		}
		usageMap = map[string]any{"input_tokens": t.usageIn, "output_tokens": t.usageOut,
			"total_tokens": t.usageIn + t.usageOut}
		output := make([]any, 0, len(t.accOrder)+2)
		if r := t.accReason.String(); r != "" {
			output = append(output, map[string]any{"id": RandomID("rs", 12),
				"type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": r}}})
		}
		if txt := t.accText.String(); txt != "" || len(t.accOrder) == 0 {
			output = append(output, map[string]any{"id": RandomID("msg", 12),
				"type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": txt,
					"annotations": []any{}}}})
		}
		for _, k := range t.accOrder {
			tc := t.accTools[k]
			if tc.Name == "" {
				continue
			}
			args := tc.Arguments
			if args == "" {
				args = "{}"
			}
			callID := tc.ID
			if callID == "" {
				callID = RandomID("call", 12)
			}
			output = append(output, map[string]any{"id": RandomID("fc", 12),
				"type": "function_call", "status": "completed",
				"call_id": callID, "name": tc.Name, "arguments": args})
		}
		completed := map[string]any{"id": id, "object": "response",
			"created_at": time.Now().Unix(), "model": t.modelName(""),
			"status": "completed", "usage": usageMap, "output": output}
		return sseFrame("response.completed",
			mustJSON(map[string]any{"type": "response.completed",
				"sequence_number": t.nextSeq(), "response": completed}))
	case ProtoMessages:
		switch stop {
		case "tool_calls":
			finish = "tool_use"
		case "length":
			finish = "max_tokens"
		case "content_filter":
			finish = "refusal"
		}
		var out []byte
		if t.msgText || t.msgReason || t.msgTool != "" {
			out = append(out, sseFrame("content_block_stop",
				mustJSON(map[string]any{"type": "content_block_stop", "index": t.msgIndex}))...)
		}
		usageMap = map[string]any{"input_tokens": t.usageIn, "output_tokens": t.usageOut}
		out = append(out, sseFrame("message_delta",
			mustJSON(map[string]any{"type": "message_delta",
				"delta": map[string]any{"stop_reason": finish, "stop_sequence": nil},
				"usage": usageMap}))...)
		out = append(out, sseFrame("message_stop", mustJSON(map[string]any{"type": "message_stop"}))...)
		return out
	default: // chat
		switch stop {
		case "tool_calls":
			finish = "tool_calls"
		case "length":
			finish = "length"
		case "content_filter":
			finish = "content_filter"
		}
		var out []byte
		out = append(out, sseData(map[string]any{
			"id": chatID(t.createdID), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": t.modelName(""),
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{},
				"finish_reason": finish}},
		})...)
		usageMap = map[string]any{"prompt_tokens": t.usageIn, "completion_tokens": t.usageOut,
			"total_tokens": t.usageIn + t.usageOut}
		out = append(out, sseData(map[string]any{
			"id": chatID(t.createdID), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": t.modelName(""),
			"choices": []any{}, "usage": usageMap,
		})...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		return out
	}
}

func (t *transcoder) emitError(msg string) []byte {
	if msg == "" {
		msg = "upstream stream failed"
	}
	switch t.to {
	case ProtoResponses:
		return sseFrame("response.failed", mustJSON(map[string]any{
			"type": "response.failed", "sequence_number": t.nextSeq(),
			"response": map[string]any{"status": "failed",
				"error": map[string]any{"code": "server_error", "message": msg}},
		}))
	case ProtoMessages:
		return sseFrame("error", mustJSON(map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": msg},
		}))
	default:
		var out []byte
		out = append(out, sseData(map[string]any{
			"error": map[string]any{"message": msg, "type": "server_error",
				"param": nil, "code": nil},
		})...)
		out = append(out, []byte("data: [DONE]\n\n")...)
		return out
	}
}

// eventsToResponseJSON folds events into one client-protocol document,
// used when the client asked for stream:false on a non-responses native.
func eventsToResponseJSON(to Protocol, events []uevent, model string) []byte {
	ir := &IRResponse{Model: model, Stop: "stop", Created: time.Now().Unix()}
	tools := map[string]*IRToolCall{}
	var order []string
	for _, ev := range events {
		switch ev.kind {
		case "created":
			if ev.createdID != "" {
				ir.ID = ev.createdID
			}
			if ev.createdModel != "" {
				ir.Model = ev.createdModel
			}
		case "text":
			ir.Text += ev.text
		case "reasoning":
			ir.Reasoning += ev.text
		case "tool":
			key := ev.toolKey
			if key == "" {
				key = "k:" + ev.toolID
			}
			tc, ok := tools[key]
			if !ok {
				tc = &IRToolCall{ID: ev.toolID, Name: ev.toolName}
				tools[key] = tc
				order = append(order, key)
			}
			if ev.toolName != "" {
				tc.Name = ev.toolName
			}
			if ev.toolID != "" {
				tc.ID = ev.toolID
			}
			tc.Arguments += ev.toolArgs
		case "done":
			if ev.stop != "" {
				ir.Stop = ev.stop
			}
		case "usage":
			if ev.inTokens != 0 {
				ir.Usage.Input = ev.inTokens
			}
			if ev.outTokens != 0 {
				ir.Usage.Output = ev.outTokens
			}
		case "error":
			ir.Stop = "error"
		}
	}
	for _, k := range order {
		if tools[k].Name != "" {
			ir.Tools = append(ir.Tools, *tools[k])
		}
	}
	if len(ir.Tools) > 0 && ir.Stop == "stop" {
		ir.Stop = "tool_calls"
	}
	out, err := marshalNoEscape(encodeResponse(to, ir))
	if err != nil {
		return []byte(`{"error":{"message":"transcode failed"}}`)
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := marshalNoEscape(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
