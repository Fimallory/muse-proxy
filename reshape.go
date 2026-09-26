package main

// reshape.go patches request tool definitions in place, in whatever
// protocol shape the payload uses:
//   - client-declared tools always win (priority), but every tool is
//     guaranteed a parameters schema (missing ones are filled in),
//   - non-object entries are dropped,
//   - synthetic definitions are appended for any missing core tool name
//     so the request still reads as an agent session upstream.
//
// Only the tools array is touched; everything else (images, reasoning
// knobs, history) is left exactly as the client sent it.

var coreToolNames = []string{"bash", "edit", "glob", "grep", "read"}

func defaultParams() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func isObjMap(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func synthTool(proto Protocol, name string) map[string]any {
	desc := "Agent tool " + name
	params := map[string]any{"type": "object", "properties": map[string]any{}}
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
			"description": desc, "parameters": params,
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

// wantsCollapse reports whether the downstream client asked for a
// non-streaming reply (stream absent or false). OpenAI defaults stream
// to false, so absence means collapse.
func wantsCollapse(payload map[string]any) bool {
	stream, _ := payload["stream"].(bool)
	return !stream
}
