package main

import "strings"

// reshape.go patches requests in place to read as from-within-OpenCode:
//   - client-declared tools always win (priority), but every tool is
//     guaranteed a parameters schema (missing ones are filled in),
//   - non-object entries are dropped,
//   - synthetic v2 definitions are appended for any missing core tool name
//     so the request still reads as an agent session upstream,
//   - the conversation-title request (opencode's title generator) is the
//     one exception: the Zen free tier only serves it with NO tools, so
//     its tools are dropped entirely instead of synthesized,
//   - Responses bodies carry v2 agent-shape fields (prompt_cache_key,
//     include, store) captured from genuine 2.0.29 traffic.
//
// Only tools + v2 body fields are touched; everything else (images,
// reasoning knobs, history) is left exactly as the client sent it.

// systemText concatenates every system-ish instruction in the payload:
// the Responses "instructions" string, an Anthropic "system" string or
// block array, and any system/developer entry in messages or input.
func systemText(p map[string]any) string {
	var sb strings.Builder
	if s, ok := p["instructions"].(string); ok {
		sb.WriteString(s)
		sb.WriteByte('\n')
	}
	switch s := p["system"].(type) {
	case string:
		sb.WriteString(s)
		sb.WriteByte('\n')
	case []any:
		for _, b := range s {
			if bm, ok := b.(map[string]any); ok {
				sb.WriteString(mstr(bm, "text"))
				sb.WriteByte('\n')
			}
		}
	}
	for _, key := range []string{"messages", "input"} {
		for _, it := range marr(p, key) {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if role := mstr(m, "role"); role != "system" && role != "developer" {
				continue
			}
			switch c := m["content"].(type) {
			case string:
				sb.WriteString(c)
				sb.WriteByte('\n')
			case []any:
				for _, part := range c {
					if pm, ok := part.(map[string]any); ok {
						sb.WriteString(mstr(pm, "text"))
						sb.WriteByte('\n')
					}
				}
			}
		}
	}
	return sb.String()
}

// titleGenNeedles identify opencode's conversation-title prompt. That
// request deliberately carries no tools, and the Zen free tier only
// accepts a tool-less request when the prompt is this exact generator;
// for every other prompt it demands tools. Both markers must be present.
var titleGenNeedles = []string{"title generator", "never use tools"}

// isTitleRequest reports whether the payload is a title-generation turn.
func isTitleRequest(p map[string]any) bool {
	text := strings.ToLower(systemText(p))
	if text == "" {
		return false
	}
	for _, n := range titleGenNeedles {
		if !strings.Contains(text, n) {
			return false
		}
	}
	return true
}

// applyTools sets the upstream tools array and reports whether the
// payload changed:
//   - a title-generation request is forwarded with no tools at all,
//   - every other request keeps its client-declared tools (client
//     priority), each guaranteed a parameters schema, with synthetic
//     definitions appended for any missing core tool name.
func applyTools(payload map[string]any, proto Protocol, titleReq bool) bool {
	if titleReq {
		if _, ok := payload["tools"]; ok {
			delete(payload, "tools")
			return true
		}
		return false
	}
	return ensureTools(payload, proto)
}

// coreToolNames mirrors the genuine v2 agent toolset (captured 2.0.29
// primary request): edit, glob, grep, question, read, shell, skill,
// subagent, webfetch, websearch, write, execute. The old v1 name "bash"
// is gone (renamed to "shell"); synthesizing it marks traffic as
// external since the 2.0.29 tightening, so it is deliberately absent.
// Client-declared tools still win, but any missing core name is appended
// so free-tier requests keep their agent shape.
var coreToolNames = []string{
	"edit", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "execute",
}

// v2ToolMeta carries genuine v2 descriptions so synthetic definitions
// read as agent tools, not minimal placeholders.
func v2ToolMeta(name string) (desc string, params map[string]any) {
	// Descriptions captured from a genuine 2.0.29 primary request.
	// shell text is OS-specific upstream ("Windows using pwsh" here);
	// the gate checks presence/shape, not the OS word, so a neutral
	// wording is used.
	switch name {
	case "edit":
		return "Edit the contents of a file by finding and replacing exact text. When editing text from Read output, preserve the exact indentation (tabs or spaces) and omit the line-number prefix.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string"},
				"oldString": map[string]any{"type": "string"},
				"newString": map[string]any{"type": "string"},
			},
			"required": []any{"path", "oldString", "newString"}, "additionalProperties": false,
		}
	case "glob":
		return "Search file paths using a glob pattern (examples: \"**/*.ts\", \"src/**/*.tsx\").", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
			},
			"required": []any{"pattern"}, "additionalProperties": false,
		}
	case "grep":
		return "Search file contents using ripgrep's regular expression syntax or literal text matching.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
				"include": map[string]any{"type": "string"},
			},
			"required": []any{"pattern"}, "additionalProperties": false,
		}
	case "question":
		return "Use this tool when you need to ask the user questions during execution.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{"type": "array"},
			},
			"required": []any{"questions"}, "additionalProperties": false,
		}
	case "read":
		return "Read the contents of a file or directory. Supports text files, images, and PDFs.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer", "minimum": 0},
				"limit":  map[string]any{"type": "integer", "minimum": 0},
			},
			"required": []any{"path"}, "additionalProperties": false,
		}
	case "shell":
		return "Execute a shell command and return its output. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string"},
				"workdir":    map[string]any{"type": "string"},
				"timeout":    map[string]any{"type": "integer", "minimum": 0},
				"background": map[string]any{"type": "boolean"},
			},
			"required": []any{"command"}, "additionalProperties": false,
		}
	case "skill":
		return "Load a specialized skill's instructions and resources into the current conversation when the task at hand matches its description.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string"},
			},
			"required": []any{"id"}, "additionalProperties": false,
		}
	case "subagent":
		return "Spawns an agent in a child session to work on the specified task.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":      map[string]any{"type": "string"},
				"description": map[string]any{"type": "string"},
			},
			"required": []any{"prompt"}, "additionalProperties": false,
		}
	case "webfetch":
		return "Fetch content from an HTTP or HTTPS URL and return it as text, markdown, or HTML.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":    map[string]any{"type": "string"},
				"format": map[string]any{"type": "string"},
			},
			"required": []any{"url"}, "additionalProperties": false,
		}
	case "websearch":
		return "Search the web using the user's selected search integration.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
			"required": []any{"query"}, "additionalProperties": false,
		}
	case "write":
		return "Writes a file to the local filesystem, overwriting if one exists.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []any{"path", "content"}, "additionalProperties": false,
		}
	case "execute":
		return "Run JavaScript in a confined Code Mode runtime to script tool calls and compose their results.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code": map[string]any{"type": "string"},
			},
			"required": []any{"code"}, "additionalProperties": false,
		}
	default:
		return "Agent tool " + name, map[string]any{"type": "object", "properties": map[string]any{}}
	}
}

func defaultParams() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func isObjMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func synthTool(proto Protocol, name string) map[string]any {
	desc, params := v2ToolMeta(name)
	switch proto {
	case ProtoChat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": desc, "parameters": params,
			},
		}
	case ProtoMessages:
		return map[string]any{
			"name": name, "description": desc, "input_schema": params,
		}
	default:
		return map[string]any{
			"type": "function", "name": name,
			"description": desc, "parameters": params, "strict": false,
		}
	}
}

func fullCoreToolset(proto Protocol) []any {
	out := make([]any, 0, len(coreToolNames))
	for _, n := range coreToolNames {
		out = append(out, synthTool(proto, n))
	}
	return out
}

// toolNameOf extracts the tool name regardless of protocol shape.
func toolNameOf(proto Protocol, item map[string]any) string {
	switch proto {
	case ProtoChat:
		if n := mstr(item, "function", "name"); n != "" {
			return n
		}
		return mstr(item, "name")
	default:
		return mstr(item, "name")
	}
}

// ensureTools normalizes payload["tools"] for the given protocol and
// reports whether the payload changed (caller must re-marshal then).
func ensureTools(payload map[string]any, proto Protocol) bool {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = fullCoreToolset(proto)
		return true
	}
	items, ok := raw.([]any)
	if !ok {
		payload["tools"] = fullCoreToolset(proto)
		return true
	}
	present := make(map[string]bool, len(items))
	kept := make([]any, 0, len(items)+len(coreToolNames))
	changed := false
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			changed = true // drop nulls and other junk
			continue
		}
		if name := toolNameOf(proto, m); name != "" {
			present[name] = true
		}
		paramsKey := "parameters"
		if proto == ProtoMessages {
			paramsKey = "input_schema"
		} else if proto == ProtoChat {
			if fn := mobj(m, "function"); fn != nil {
				if p, ok := fn["parameters"]; !ok || !isObjMap(p) {
					fn["parameters"] = defaultParams()
					changed = true
				}
				kept = append(kept, m)
				continue
			}
		}
		if p, ok := m[paramsKey]; !ok || !isObjMap(p) {
			m[paramsKey] = defaultParams()
			changed = true
		}
		kept = append(kept, m)
	}
	for _, n := range coreToolNames {
		if !present[n] {
			kept = append(kept, synthTool(proto, n))
			changed = true
		}
	}
	if !changed {
		return false
	}
	payload["tools"] = kept
	return true
}

// ensureResponseTools keeps the old name working for the responses path.
func ensureResponseTools(payload map[string]any) bool {
	return ensureTools(payload, ProtoResponses)
}

// ensureV2ResponsesFields guarantees the Responses agent-shape fields a
// genuine v2 client always sends (captured 2.0.29, both title with no
// tools and primary with tools):
//
//	prompt_cache_key = session, include = ["reasoning.encrypted_content"],
//	store = false (when absent).
//
// Returns true when the payload changed. Non-Responses payloads are
// untouched.
func ensureV2ResponsesFields(payload map[string]any, session string) bool {
	if payload == nil || session == "" {
		return false
	}
	// Only Responses bodies carry these fields. Detect by shape: has
	// "input" (Responses) rather than "messages" (Chat/Messages).
	if _, ok := payload["input"]; !ok {
		return false
	}
	changed := false
	if v, _ := payload["prompt_cache_key"].(string); v != session {
		// Genuine v2 always sets prompt_cache_key to the session, even
		// for title requests. A stale v1 client omits it: fill it.
		// If a client sends a different session key, align it — the
		// headers already carry session, and mismatch reads as external.
		payload["prompt_cache_key"] = session
		changed = true
	}
	if _, ok := payload["include"]; !ok {
		payload["include"] = []any{"reasoning.encrypted_content"}
		changed = true
	}
	if _, ok := payload["store"]; !ok {
		payload["store"] = false
		changed = true
	}
	return changed
}

// carryV2Fields copies v2-only body fields through protocol conversion
// so a converted request keeps its from-within-OpenCode shape.
func carryV2Fields(src map[string]any, from Protocol, dst map[string]any, to Protocol) {
	if src == nil || dst == nil {
		return
	}
	// prompt_cache_key / include / store only exist on Responses.
	if to == ProtoResponses {
		if v, ok := src["prompt_cache_key"]; ok {
			dst["prompt_cache_key"] = v
		}
		if v, ok := src["include"]; ok {
			dst["include"] = v
		}
		if v, ok := src["store"]; ok {
			dst["store"] = v
		}
	}
	// max_output_tokens <-> max_tokens carry-through when the IR drops
	// them (e.g. Chat max_completion_tokens variants).
	if to == ProtoResponses {
		if _, ok := dst["max_output_tokens"]; !ok {
			if f, ok := mnum(src, "max_tokens"); ok {
				dst["max_output_tokens"] = int(f)
			} else if f, ok := mnum(src, "max_completion_tokens"); ok {
				dst["max_output_tokens"] = int(f)
			}
		}
	}
	_ = from
}

// wantsCollapse reports whether the downstream client asked for a
// non-streaming reply (stream absent or false). OpenAI defaults stream
// to false, so absence means collapse.
func wantsCollapse(payload map[string]any) bool {
	stream, _ := payload["stream"].(bool)
	return !stream
}
