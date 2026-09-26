package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Identity helpers. The session/project shapes mirror what the Zen free tier
// accepts: ses_<12 hex><14 base62> and prj_<24 hex>. Anything else is hashed
// into shape instead of being passed through.

// canonicalSessionPattern matches OpenCode's canonical session format.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CanonicalSessionID returns signal unchanged when it already carries an
// official session shape (preserving upstream prompt-cache affinity).
// Any other identity is deterministically hashed into the canonical shape
// so the same conversation always maps to the same session.
func CanonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := hex.EncodeToString(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// StableID derives a stable prefixed hex ID from a signal.
func StableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

// RandomID mints a fresh prefixed hex ID.
func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// extractTexts pulls the human/model language out of a request body in
// any of the three protocols: instructions/system plus message text.
// Tool definitions and tool-call traces are deliberately excluded so the
// hash keys on conversation content, not on agent activity.
func extractTexts(body []byte) (instructions, input string) {
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		return "", ""
	}
	if s, ok := p["instructions"].(string); ok {
		instructions = s
	}
	if s, ok := p["system"].(string); ok && instructions == "" {
		instructions = s
	}
	input = extractInput(p["input"])
	if input == "" {
		// Chat- or Messages-shaped body.
		input = extractMessages(p["messages"])
	}
	return instructions, input
}

func extractInput(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, item := range t {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// Message items: collect text parts only.
			switch c := m["content"].(type) {
			case string:
				sb.WriteString(c)
				sb.WriteString("\n")
			case []any:
				for _, part := range c {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					typ, _ := pm["type"].(string)
					switch typ {
					case "input_text", "text", "output_text", "summary_text":
						if s, ok := pm["text"].(string); ok {
							sb.WriteString(s)
							sb.WriteString("\n")
						}
					}
				}
			}
			// Reasoning items carry summary text worth hashing.
			if m["type"] == "reasoning" {
				if summary, ok := m["summary"].([]any); ok {
					for _, s := range summary {
						if sm, ok := s.(map[string]any); ok {
							if txt, ok := sm["text"].(string); ok {
								sb.WriteString(txt)
								sb.WriteString("\n")
							}
						}
					}
				}
			}
		}
		return sb.String()
	}
	return ""
}

func extractMessages(v any) string {
	items, ok := v.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["content"].(string); ok {
			sb.WriteString(s)
			sb.WriteString("\n")
			continue
		}
		for _, part := range marr(m, "content") {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text", "input_text", "output_text":
				if s, ok := pm["text"].(string); ok {
					sb.WriteString(s)
					sb.WriteString("\n")
				}
			}
		}
	}
	return sb.String()
}

// contentHash hashes the first n characters (runes) of the request text.
// Only the hex digest is ever stored; content never leaves the request.
func contentHash(instructions, input string, n int) string {
	r := []rune(instructions + "\n" + input)
	if len(r) > n {
		r = r[:n]
	}
	sum := sha256.Sum256([]byte(string(r)))
	return hex.EncodeToString(sum[:])
}

// jsonStringAt reads a nested string field from a decoded body.
func jsonStringAt(body map[string]any, path ...string) string {
	var cur any = body
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	s, _ := cur.(string)
	return s
}
