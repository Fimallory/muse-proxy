package main

import (
	"encoding/json"
	"strings"
)

// convert.go translates requests between Chat, Responses and Messages
// through one small intermediate representation. Same-protocol traffic
// never touches this file (it stays byte-transparent in proxy.go).

// IRPart is one content unit inside a message.
type IRPart struct {
	Kind string // "text" | "image" | "tool_call" | "tool_result" | "reasoning"
	Text string
	// image:
	ImageURL  string // data: or https: URL
	MediaType string
	// tool_call:
	CallID, ToolName, Arguments string
	// tool_result:
	ResultFor string
	IsError   bool
}

// IRMessage is a conversation turn. Role is user, assistant or tool.
type IRMessage struct {
	Role  string
	Parts []IRPart
}

// IRTool is a function tool both sides understand.
type IRTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// IRRequest is the protocol-neutral request.
type IRRequest struct {
	Model           string
	System          string
	Messages        []IRMessage
	Tools           []IRTool
	ToolChoice      string // "auto" | "required" | "none" | ""
	NamedTool       string
	Stream          bool
	Temperature     *float64
	TopP            *float64
	MaxTokens       int
	ReasoningEffort string // "", minimal|low|medium|high|xhigh|max|none
	Store           *bool
	Metadata        map[string]any
}

// ---------- generic accessors ----------

func mstr(m map[string]any, keys ...string) string {
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

func mnum(m map[string]any, keys ...string) (float64, bool) {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur = mm[k]
	}
	f, ok := cur.(float64)
	return f, ok
}

func marr(m map[string]any, keys ...string) []any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	a, _ := cur.([]any)
	return a
}

func mobj(m map[string]any, keys ...string) map[string]any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	o, _ := cur.(map[string]any)
	return o
}

func mbool(m map[string]any, keys ...string) (bool, bool) {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return false, false
		}
		cur = mm[k]
	}
	b, ok := cur.(bool)
	return b, ok
}

func defaultSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ---------- reasoning effort <-> thinking budget ----------

// budgetForEffort returns the lower bound budget of an effort level.
func budgetForEffort(e string) int {
	switch strings.ToLower(e) {
	case "minimal", "low":
		return 1024
	case "medium":
		return 4096
	case "high":
		return 8192
	case "xhigh":
		return 16384
	case "max":
		return 32768
	default:
		return 4096
	}
}

// effortForBudget maps a thinking budget back to an effort level.
func effortForBudget(b int) string {
	switch {
	case b <= 0:
		return "high"
	case b >= 32768:
		return "max"
	case b >= 16384:
		return "xhigh"
	case b >= 8192:
		return "high"
	case b > 2048:
		return "medium"
	default:
		return "low"
	}
}

// splitDataURL parses "data:<media>;base64,<data>".
func splitDataURL(u string) (media, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := u[len("data:"):]
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	meta, data := parts[0], parts[1]
	media = strings.SplitN(meta, ";", 2)[0]
	if media == "" {
		media = "application/octet-stream"
	}
	return media, data, true
}

// ---------- Chat ----------

func decodeChatRequest(p map[string]any) *IRRequest {
	ir := &IRRequest{Model: mstr(p, "model")}
	if s, ok := mbool(p, "stream"); ok {
		ir.Stream = s
	}
	if f, ok := mnum(p, "temperature"); ok {
		ir.Temperature = &f
	}
	if f, ok := mnum(p, "top_p"); ok {
		ir.TopP = &f
	}
	if f, ok := mnum(p, "max_tokens"); ok {
		ir.MaxTokens = int(f)
	} else if f, ok := mnum(p, "max_completion_tokens"); ok {
		ir.MaxTokens = int(f)
	}
	ir.ReasoningEffort = mstr(p, "reasoning_effort")
	if b, ok := mbool(p, "store"); ok {
		ir.Store = &b
	}
	if md, ok := p["metadata"].(map[string]any); ok {
		ir.Metadata = md
	}
	var sys []string
	for _, raw := range marr(p, "messages") {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if s, ok := m["content"].(string); ok && s != "" {
				sys = append(sys, s)
			}
		case "user":
			msg := IRMessage{Role: "user"}
			switch c := m["content"].(type) {
			case string:
				msg.Parts = append(msg.Parts, IRPart{Kind: "text", Text: c})
			case []any:
				for _, part := range c {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					switch pm["type"] {
					case "text":
						if s, ok := pm["text"].(string); ok {
							msg.Parts = append(msg.Parts, IRPart{Kind: "text", Text: s})
						}
					case "image_url":
						u := mstr(pm, "image_url", "url")
						if u == "" {
							u, _ = pm["image_url"].(string)
						}
						if u != "" {
							msg.Parts = append(msg.Parts, IRPart{Kind: "image", ImageURL: u})
						}
					}
				}
			}
			ir.Messages = append(ir.Messages, msg)
		case "assistant":
			msg := IRMessage{Role: "assistant"}
			if s, ok := m["content"].(string); ok && s != "" {
				msg.Parts = append(msg.Parts, IRPart{Kind: "text", Text: s})
			}
			for _, tc := range marr(m, "tool_calls") {
				tm, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tm["id"].(string)
				msg.Parts = append(msg.Parts, IRPart{
					Kind: "tool_call", CallID: id,
					ToolName:  mstr(tm, "function", "name"),
					Arguments: mstr(tm, "function", "arguments"),
				})
			}
			ir.Messages = append(ir.Messages, msg)
		case "tool":
			content := ""
			switch c := m["content"].(type) {
			case string:
				content = c
			default:
				if b, err := json.Marshal(c); err == nil {
					content = string(b)
				}
			}
			ir.Messages = append(ir.Messages, IRMessage{Role: "tool", Parts: []IRPart{{
				Kind: "tool_result", ResultFor: mstr(m, "tool_call_id"), Text: content,
			}}})
		}
	}
	ir.System = strings.Join(sys, "\n")
	for _, raw := range marr(p, "tools") {
		tm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn := mobj(tm, "function")
		name := mstr(fn, "name")
		if name == "" {
			name, _ = tm["name"].(string)
		}
		if name == "" {
			continue
		}
		schema := mobj(fn, "parameters")
		if schema == nil {
			schema = mobj(tm, "parameters")
		}
		if schema == nil {
			schema = defaultSchema()
		}
		ir.Tools = append(ir.Tools, IRTool{
			Name:        name,
			Description: mstr(fn, "description"),
			Schema:      schema,
		})
	}
	switch tc := p["tool_choice"].(type) {
	case string:
		ir.ToolChoice = tc
	case map[string]any:
		if mstr(tc, "type") == "function" {
			ir.ToolChoice = "named"
			ir.NamedTool = mstr(tc, "function", "name")
		}
	}
	return ir
}

func encodeChatRequest(ir *IRRequest) map[string]any {
	out := map[string]any{"model": ir.Model}
	msgs := make([]any, 0, len(ir.Messages)+1)
	if ir.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": ir.System})
	}
	for _, m := range ir.Messages {
		switch m.Role {
		case "user":
			var texts []string
			var parts []any
			for _, part := range m.Parts {
				switch part.Kind {
				case "text":
					texts = append(texts, part.Text)
				case "image":
					parts = append(parts, map[string]any{
						"type": "image_url", "image_url": map[string]any{"url": part.ImageURL},
					})
				}
			}
			if len(parts) == 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
			} else {
				if t := strings.Join(texts, "\n"); t != "" {
					parts = append([]any{map[string]any{"type": "text", "text": t}}, parts...)
				}
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			}
		case "assistant":
			var texts []string
			var calls []any
			for _, part := range m.Parts {
				switch part.Kind {
				case "text":
					texts = append(texts, part.Text)
				case "reasoning":
					// Chat has no reasoning slot; providers carrying the
					// extension read reasoning_content.
					texts = append(texts, part.Text)
				case "tool_call":
					args := part.Arguments
					if args == "" {
						args = "{}"
					}
					call := map[string]any{
						"type":     "function",
						"function": map[string]any{"name": part.ToolName, "arguments": args},
					}
					if part.CallID != "" {
						call["id"] = part.CallID
					}
					calls = append(calls, call)
				}
			}
			am := map[string]any{"role": "assistant"}
			if t := strings.Join(texts, "\n"); t != "" {
				am["content"] = t
			} else {
				am["content"] = nil
			}
			if len(calls) > 0 {
				am["tool_calls"] = calls
			}
			msgs = append(msgs, am)
		case "tool":
			for _, part := range m.Parts {
				if part.Kind != "tool_result" {
					continue
				}
				msgs = append(msgs, map[string]any{
					"role": "tool", "tool_call_id": part.ResultFor, "content": part.Text,
				})
			}
		}
	}
	out["messages"] = msgs
	if len(ir.Tools) > 0 {
		tools := make([]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.Schema,
				},
			})
		}
		out["tools"] = tools
	}
	switch ir.ToolChoice {
	case "named":
		if ir.NamedTool != "" {
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": ir.NamedTool}}
		}
	case "auto", "required", "none":
		out["tool_choice"] = ir.ToolChoice
	}
	if ir.ReasoningEffort != "" && ir.ReasoningEffort != "none" {
		out["reasoning_effort"] = ir.ReasoningEffort
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	if ir.MaxTokens > 0 {
		out["max_tokens"] = ir.MaxTokens
	}
	out["stream"] = ir.Stream
	if ir.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out
}

// ---------- Responses ----------

func decodeResponsesRequest(p map[string]any) *IRRequest {
	ir := &IRRequest{Model: mstr(p, "model")}
	if s, ok := mbool(p, "stream"); ok {
		ir.Stream = s
	}
	if f, ok := mnum(p, "temperature"); ok {
		ir.Temperature = &f
	}
	if f, ok := mnum(p, "top_p"); ok {
		ir.TopP = &f
	}
	if f, ok := mnum(p, "max_output_tokens"); ok {
		ir.MaxTokens = int(f)
	}
	ir.ReasoningEffort = mstr(p, "reasoning", "effort")
	if b, ok := mbool(p, "store"); ok {
		ir.Store = &b
	}
	if md, ok := p["metadata"].(map[string]any); ok {
		ir.Metadata = md
	}
	ir.System = mstr(p, "instructions")
	emitText := func(role, text string) {
		if text == "" {
			return
		}
		ir.Messages = append(ir.Messages, IRMessage{Role: role, Parts: []IRPart{{Kind: "text", Text: text}}})
	}
	switch in := p["input"].(type) {
	case string:
		emitText("user", in)
	case []any:
		for _, raw := range in {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "message":
				role, _ := item["role"].(string)
				if role == "system" || role == "developer" {
					for _, c := range marr(item, "content") {
						pm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						if t, _ := pm["type"].(string); t == "input_text" || t == "text" {
							if s, ok := pm["text"].(string); ok {
								ir.System += s + "\n"
							}
						}
					}
					continue
				}
				if role != "user" && role != "assistant" {
					continue
				}
				msg := IRMessage{Role: role}
				for _, c := range marr(item, "content") {
					pm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					switch pm["type"] {
					case "input_text", "output_text", "text":
						if s, ok := pm["text"].(string); ok {
							msg.Parts = append(msg.Parts, IRPart{Kind: "text", Text: s})
						}
					case "input_image":
						if u, ok := pm["image_url"].(string); ok && u != "" {
							msg.Parts = append(msg.Parts, IRPart{Kind: "image", ImageURL: u})
						}
					}
				}
				if len(msg.Parts) > 0 {
					ir.Messages = append(ir.Messages, msg)
				}
			case "function_call":
				callID, _ := item["call_id"].(string)
				if callID == "" {
					callID, _ = item["id"].(string)
				}
				args, _ := item["arguments"].(string)
				ir.Messages = append(ir.Messages, IRMessage{Role: "assistant", Parts: []IRPart{{
					Kind: "tool_call", CallID: callID, ToolName: mstr(item, "name"), Arguments: args,
				}}})
			case "function_call_output":
				callID, _ := item["call_id"].(string)
				var text string
				switch o := item["output"].(type) {
				case string:
					text = o
				default:
					if b, err := json.Marshal(o); err == nil {
						text = string(b)
					}
				}
				ir.Messages = append(ir.Messages, IRMessage{Role: "tool", Parts: []IRPart{{
					Kind: "tool_result", ResultFor: callID, Text: text,
				}}})
			case "reasoning":
				var texts []string
				for _, s := range marr(item, "summary") {
					if sm, ok := s.(map[string]any); ok {
						if t, ok := sm["text"].(string); ok && t != "" {
							texts = append(texts, t)
						}
					}
				}
				if len(texts) > 0 {
					ir.Messages = append(ir.Messages, IRMessage{Role: "assistant", Parts: []IRPart{{
						Kind: "reasoning", Text: strings.Join(texts, "\n"),
					}}})
				}
			}
		}
	}
	for _, raw := range marr(p, "tools") {
		tm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		schema := mobj(tm, "parameters")
		if schema == nil {
			schema = defaultSchema()
		}
		ir.Tools = append(ir.Tools, IRTool{
			Name: name, Description: mstr(tm, "description"), Schema: schema,
		})
	}
	switch tc := p["tool_choice"].(type) {
	case string:
		ir.ToolChoice = tc
	case map[string]any:
		if mstr(tc, "type") == "function" {
			ir.ToolChoice = "named"
			ir.NamedTool = mstr(tc, "name")
		}
	}
	ir.System = strings.TrimRight(ir.System, "\n")
	return ir
}

func encodeResponsesRequest(ir *IRRequest) map[string]any {
	out := map[string]any{"model": ir.Model}
	if ir.System != "" {
		out["instructions"] = ir.System
	}
	input := make([]any, 0, len(ir.Messages))
	for _, m := range ir.Messages {
		switch m.Role {
		case "user", "assistant":
			content := make([]any, 0, len(m.Parts))
			ctype := "input_text"
			if m.Role == "assistant" {
				ctype = "output_text"
			}
			for _, part := range m.Parts {
				switch part.Kind {
				case "text":
					content = append(content, map[string]any{"type": ctype, "text": part.Text})
				case "image":
					content = append(content, map[string]any{"type": "input_image", "image_url": part.ImageURL})
				case "reasoning":
					input = append(input, map[string]any{
						"type":    "reasoning",
						"summary": []any{map[string]any{"type": "summary_text", "text": part.Text}},
					})
				case "tool_call":
					id := part.CallID
					if id == "" {
						id = RandomID("call", 12)
					}
					args := part.Arguments
					if args == "" {
						args = "{}"
					}
					input = append(input, map[string]any{
						"type": "function_call", "id": id, "call_id": id,
						"name": part.ToolName, "arguments": args,
					})
				}
			}
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": m.Role, "content": content})
			}
		case "tool":
			for _, part := range m.Parts {
				if part.Kind != "tool_result" {
					continue
				}
				callID := part.ResultFor
				if callID == "" {
					callID = RandomID("call", 12)
				}
				input = append(input, map[string]any{
					"type": "function_call_output", "call_id": callID, "output": part.Text,
				})
			}
		}
	}
	out["input"] = input
	if len(ir.Tools) > 0 {
		tools := make([]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tools = append(tools, map[string]any{
				"type": "function", "name": t.Name,
				"description": t.Description, "parameters": t.Schema,
			})
		}
		out["tools"] = tools
	}
	switch ir.ToolChoice {
	case "named":
		if ir.NamedTool != "" {
			out["tool_choice"] = map[string]any{"type": "function", "name": ir.NamedTool}
		}
	case "auto", "required", "none":
		out["tool_choice"] = ir.ToolChoice
	}
	if ir.ReasoningEffort != "" && ir.ReasoningEffort != "none" {
		out["reasoning"] = map[string]any{"effort": ir.ReasoningEffort}
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	if ir.MaxTokens > 0 {
		out["max_output_tokens"] = ir.MaxTokens
	}
	out["stream"] = ir.Stream
	if ir.Store != nil {
		out["store"] = *ir.Store
	}
	if ir.Metadata != nil {
		out["metadata"] = ir.Metadata
	}
	return out
}

// ---------- Messages (Anthropic) ----------

func decodeMessagesRequest(p map[string]any) *IRRequest {
	ir := &IRRequest{Model: mstr(p, "model")}
	if s, ok := mbool(p, "stream"); ok {
		ir.Stream = s
	}
	if f, ok := mnum(p, "temperature"); ok {
		ir.Temperature = &f
	}
	if f, ok := mnum(p, "top_p"); ok {
		ir.TopP = &f
	}
	if f, ok := mnum(p, "max_tokens"); ok {
		ir.MaxTokens = int(f)
	}
	if md, ok := p["metadata"].(map[string]any); ok {
		ir.Metadata = md
	}
	// Explicit effort wins over budget.
	if e := mstr(p, "output_config", "effort"); e != "" {
		ir.ReasoningEffort = e
	} else if e := mstr(p, "effort"); e != "" {
		ir.ReasoningEffort = e
	} else if th := mobj(p, "thinking"); th != nil {
		if typ, _ := th["type"].(string); typ == "enabled" {
			if b, ok := th["budget_tokens"].(float64); ok {
				ir.ReasoningEffort = effortForBudget(int(b))
			} else {
				ir.ReasoningEffort = "high"
			}
		}
	}
	switch s := p["system"].(type) {
	case string:
		ir.System = s
	case []any:
		var parts []string
		for _, b := range s {
			if bm, ok := b.(map[string]any); ok {
				if t, _ := bm["type"].(string); t == "text" {
					if txt, ok := bm["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		ir.System = strings.Join(parts, "\n")
	}
	for _, raw := range marr(p, "messages") {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			continue
		}
		msg := IRMessage{Role: role}
		flush := func() {
			if len(msg.Parts) > 0 {
				ir.Messages = append(ir.Messages, msg)
				msg = IRMessage{Role: role}
			}
		}
		addText := func(s string) {
			if s != "" {
				msg.Parts = append(msg.Parts, IRPart{Kind: "text", Text: s})
			}
		}
		switch c := m["content"].(type) {
		case string:
			addText(c)
		case []any:
			for _, b := range c {
				bm, ok := b.(map[string]any)
				if !ok {
					continue
				}
				switch bm["type"] {
				case "text":
					addText(mstr(bm, "text"))
				case "image":
					src := mobj(bm, "source")
					switch mstr(src, "type") {
					case "base64":
						media := mstr(src, "media_type")
						data, _ := src["data"].(string)
						if media == "" {
							media = "image/png"
						}
						msg.Parts = append(msg.Parts, IRPart{
							Kind: "image", ImageURL: "data:" + media + ";base64," + data, MediaType: media,
						})
					case "url":
						if u := mstr(src, "url"); u != "" {
							msg.Parts = append(msg.Parts, IRPart{Kind: "image", ImageURL: u})
						}
					}
				case "tool_use":
					var args string
					switch in := bm["input"].(type) {
					case string:
						args = in
					default:
						if b, err := json.Marshal(in); err == nil {
							args = string(b)
						} else {
							args = "{}"
						}
					}
					msg.Parts = append(msg.Parts, IRPart{
						Kind: "tool_call", CallID: mstr(bm, "id"),
						ToolName: mstr(bm, "name"), Arguments: args,
					})
				case "tool_result":
					var text string
					switch ct := bm["content"].(type) {
					case string:
						text = ct
					case []any:
						var parts []string
						for _, cb := range ct {
							if cm, ok := cb.(map[string]any); ok {
								if t, _ := cm["type"].(string); t == "text" {
									parts = append(parts, mstr(cm, "text"))
								}
							}
						}
						text = strings.Join(parts, "\n")
					default:
						if b, err := json.Marshal(ct); err == nil {
							text = string(b)
						}
					}
					isErr, _ := bm["is_error"].(bool)
					// Tool results live in their own turn: flush anything
					// accumulated so far so text and results never fuse.
					flush()
					ir.Messages = append(ir.Messages, IRMessage{Role: "tool", Parts: []IRPart{{
						Kind: "tool_result", ResultFor: mstr(bm, "tool_use_id"),
						Text: text, IsError: isErr,
					}}})
				}
			}
		}
		flush()
	}
	for _, raw := range marr(p, "tools") {
		tm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		schema := mobj(tm, "input_schema")
		if schema == nil {
			schema = defaultSchema()
		}
		ir.Tools = append(ir.Tools, IRTool{
			Name: name, Description: mstr(tm, "description"), Schema: schema,
		})
	}
	switch tc := p["tool_choice"].(type) {
	case map[string]any:
		switch mstr(tc, "type") {
		case "auto":
			ir.ToolChoice = "auto"
		case "any":
			ir.ToolChoice = "required"
		case "tool":
			ir.ToolChoice = "named"
			ir.NamedTool = mstr(tc, "name")
		}
	case string:
		if tc == "required" {
			ir.ToolChoice = "required"
		}
	}
	return ir
}

func encodeMessagesRequest(ir *IRRequest) map[string]any {
	out := map[string]any{"model": ir.Model}
	if ir.System != "" {
		out["system"] = ir.System
	}
	// Anthropic rejects consecutive same-role messages: merge them.
	type acc struct {
		role  string
		parts []IRPart
	}
	var merged []acc
	for _, m := range ir.Messages {
		role := m.Role
		if role == "tool" {
			// Tool turns ride as user turns carrying tool_result blocks.
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		// Normalize tool_result parts that arrived on a tool turn.
		parts := append([]IRPart{}, m.Parts...)
		if m.Role == "tool" {
			for i := range parts {
				if parts[i].Kind == "text" {
					parts[i].Kind = "tool_result"
				}
			}
		}
		if len(merged) > 0 && merged[len(merged)-1].role == role {
			merged[len(merged)-1].parts = append(merged[len(merged)-1].parts, parts...)
		} else {
			merged = append(merged, acc{role: role, parts: append([]IRPart{}, parts...)})
		}
	}
	msgs := make([]any, 0, len(merged))
	for _, m := range merged {
		content := make([]any, 0, len(m.parts))
		for _, part := range m.parts {
			switch part.Kind {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			case "reasoning":
				content = append(content, map[string]any{"type": "text", "text": part.Text})
			case "image":
				if media, data, ok := splitDataURL(part.ImageURL); ok {
					content = append(content, map[string]any{"type": "image", "source": map[string]any{
						"type": "base64", "media_type": media, "data": data,
					}})
				} else if part.ImageURL != "" {
					content = append(content, map[string]any{"type": "image", "source": map[string]any{
						"type": "url", "url": part.ImageURL,
					}})
				}
			case "tool_call":
				var input any = map[string]any{}
				if part.Arguments != "" {
					var v any
					if err := json.Unmarshal([]byte(part.Arguments), &v); err == nil {
						input = v
					} else {
						input = part.Arguments
					}
				}
				id := part.CallID
				if id == "" {
					id = RandomID("toolu", 12)
				}
				content = append(content, map[string]any{
					"type": "tool_use", "id": id, "name": part.ToolName, "input": input,
				})
			case "tool_result":
				var c any = part.Text
				tr := map[string]any{"type": "tool_result", "tool_use_id": part.ResultFor, "content": c}
				if part.IsError {
					tr["is_error"] = true
				}
				content = append(content, tr)
			}
		}
		if len(content) == 0 {
			content = append(content, map[string]any{"type": "text", "text": ""})
		}
		msgs = append(msgs, map[string]any{"role": m.role, "content": content})
	}
	out["messages"] = msgs
	// Anthropic has no tool_choice "none": omitting tools is the equivalent.
	if len(ir.Tools) > 0 && ir.ToolChoice != "none" {
		tools := make([]any, 0, len(ir.Tools))
		for _, t := range ir.Tools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "input_schema": t.Schema,
			})
		}
		out["tools"] = tools
	}
	switch ir.ToolChoice {
	case "auto":
		out["tool_choice"] = map[string]any{"type": "auto"}
	case "required":
		out["tool_choice"] = map[string]any{"type": "any"}
	case "named":
		if ir.NamedTool != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": ir.NamedTool}
		}
	}
	if ir.ReasoningEffort != "" && ir.ReasoningEffort != "none" {
		budget := budgetForEffort(ir.ReasoningEffort)
		out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
		maxT := ir.MaxTokens
		if maxT <= 0 {
			maxT = 4096
		}
		if maxT <= budget {
			maxT = budget + 4096
		}
		out["max_tokens"] = maxT
	} else if ir.MaxTokens > 0 {
		out["max_tokens"] = ir.MaxTokens
	} else {
		out["max_tokens"] = 4096
	}
	if ir.Temperature != nil {
		out["temperature"] = *ir.Temperature
	}
	if ir.TopP != nil {
		out["top_p"] = *ir.TopP
	}
	out["stream"] = ir.Stream
	if ir.Metadata != nil {
		out["metadata"] = ir.Metadata
	}
	return out
}

// decodeRequest routes a raw body to the right decoder by client protocol.
func decodeRequest(proto Protocol, p map[string]any) *IRRequest {
	switch proto {
	case ProtoChat:
		return decodeChatRequest(p)
	case ProtoResponses:
		return decodeResponsesRequest(p)
	case ProtoMessages:
		return decodeMessagesRequest(p)
	default:
		return decodeChatRequest(p)
	}
}

// encodeRequest renders the IR into the target protocol.
func encodeRequest(proto Protocol, ir *IRRequest) map[string]any {
	switch proto {
	case ProtoChat:
		return encodeChatRequest(ir)
	case ProtoResponses:
		return encodeResponsesRequest(ir)
	case ProtoMessages:
		return encodeMessagesRequest(ir)
	default:
		return encodeChatRequest(ir)
	}
}
