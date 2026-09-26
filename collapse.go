package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// collapse.go folds a Responses-protocol SSE stream into a single response
// object for downstream clients that asked for stream:false.
//
// The upstream free tier only serves streaming requests, so the gateway
// forces stream:true on the wire and collapses here. The terminal
// response.completed event already carries the authoritative response
// object, which is returned verbatim; only a truncated stream is
// synthesized from accumulated deltas.

const collapseMaxBytes = 64 << 20

type collapseTool struct {
	id     string
	callID string
	name   string
	args   strings.Builder
	done   bool
}

// collapseSSE folds raw SSE bytes into one Responses JSON object.
// It returns the object bytes and the HTTP status to answer with.
func collapseSSE(body []byte, model, fallbackID string) ([]byte, int) {
	var (
		respID, respModel string
		createdAt         int64
		text              strings.Builder
		textFinal         string
		hasTextFinal      bool
		tools             []*collapseTool
		byItem            = map[string]*collapseTool{}
		sawDone           bool
	)

	toolFor := func(itemID string) *collapseTool {
		if t, ok := byItem[itemID]; ok {
			return t
		}
		if len(tools) > 0 {
			return tools[len(tools)-1]
		}
		return nil
	}

	for _, frame := range splitSSEFrames(body) {
		data := frameData(frame)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		typ, _ := ev["type"].(string)
		strAt := func(m map[string]any, keys ...string) string {
			var cur any = m
			for _, k := range keys {
				mm, ok := cur.(map[string]any)
				if !ok {
					return ""
				}
				cur = mm[k]
			}
			s, _ := cur.(string)
			return s
		}
		switch typ {
		case "response.created":
			respID = strAt(ev, "response", "id")
			respModel = strAt(ev, "response", "model")
			if f, ok := ev["response"].(map[string]any)["created_at"].(float64); ok {
				createdAt = int64(f)
			}
		case "response.output_text.delta":
			text.WriteString(strAt(ev, "delta"))
		case "response.output_text.done":
			if t := strAt(ev, "text"); t != "" {
				textFinal = t
				hasTextFinal = true
			}
			sawDone = true
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]any)
			if item == nil {
				break
			}
			if itype, _ := item["type"].(string); itype == "function_call" {
				t := &collapseTool{}
				if id, ok := item["id"].(string); ok {
					t.id = id
					byItem[id] = t
				}
				if cid, ok := item["call_id"].(string); ok {
					t.callID = cid
					byItem[cid] = t
				}
				t.name, _ = item["name"].(string)
				if args, ok := item["arguments"].(string); ok {
					t.args.WriteString(args)
				}
				tools = append(tools, t)
			}
		case "response.function_call_arguments.delta":
			if t := toolFor(strAt(ev, "item_id")); t != nil {
				t.args.WriteString(strAt(ev, "delta"))
			}
		case "response.function_call_arguments.done":
			t := toolFor(strAt(ev, "item_id"))
			if t == nil {
				break
			}
			if args := strAt(ev, "arguments"); args != "" {
				t.args.Reset()
				t.args.WriteString(args)
			}
			t.done = true
			sawDone = true
		case "response.output_item.done":
			sawDone = true
		case "response.completed", "response.failed", "response.incomplete":
			if resp, ok := ev["response"]; ok {
				if out, err := marshalNoEscape(resp); err == nil {
					return out, 200
				}
			}
		case "error":
			if e, ok := ev["error"]; ok {
				if out, err := marshalNoEscape(map[string]any{"error": e}); err == nil {
					return out, 502
				}
			}
		}
	}

	// Truncated stream: synthesize from whatever accumulated.
	finalText := text.String()
	if hasTextFinal {
		finalText = textFinal
	}
	status := "incomplete"
	if sawDone {
		status = "completed"
	}
	if respID == "" {
		respID = fallbackID
	}
	if respID == "" {
		respID = RandomID("resp", 12)
	}
	if respModel == "" {
		respModel = model
	}
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	output := make([]any, 0, len(tools)+1)
	if finalText != "" {
		output = append(output, map[string]any{
			"id":     RandomID("msg", 12),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": finalText, "annotations": []any{},
			}},
		})
	}
	for _, t := range tools {
		if t.name == "" {
			continue
		}
		st := "in_progress"
		if t.done {
			st = "completed"
		}
		output = append(output, map[string]any{
			"id": t.id, "type": "function_call", "status": st,
			"call_id": t.callID, "name": t.name, "arguments": t.args.String(),
		})
	}
	obj := map[string]any{
		"id": respID, "object": "response", "created_at": createdAt,
		"model": respModel, "status": status, "output": output,
	}
	out, err := marshalNoEscape(obj)
	if err != nil {
		return []byte(`{"error":{"message":"collapse failed"}}`), 502
	}
	return out, 200
}

// splitSSEFrames cuts raw SSE bytes into frames on blank lines.
func splitSSEFrames(body []byte) [][]byte {
	body = bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	parts := bytes.Split(body, []byte("\n\n"))
	frames := make([][]byte, 0, len(parts))
	for _, p := range parts {
		if len(bytes.TrimSpace(p)) == 0 {
			continue
		}
		frames = append(frames, p)
	}
	return frames
}

// frameData concatenates the data: lines of one SSE frame.
func frameData(frame []byte) string {
	var sb strings.Builder
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		if rest, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.Write(bytes.TrimLeft(rest, " "))
		}
	}
	return sb.String()
}

// marshalNoEscape encodes JSON without escaping <, > and & so that
// re-marshaled request bodies stay byte-close to what the client sent
// (base64 and URLs are unaffected either way).
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
