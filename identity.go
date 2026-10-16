package main

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Identity helpers. Session/project shapes mirror what the Zen free tier
// accepts in v2 (captured 2.0.29):
//   session: ses_<12 hex><14 base62> with a tiny embedded counter
//     (prompt_cache_key carries the same)
//   project: 40 hex, no prefix (e.g. 513945e107b3e9bacde70050e755d4d65a49230d;
//     32 hex is accepted as well)
// Anything else is re-minted (sessions) or hashed (projects) into shape
// instead of being passed through.

// canonicalSessionPattern matches OpenCode's canonical session format.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// projectPattern matches the v2 project shapes observed live: 40 hex
// (what genuine clients send) and 32 hex (accepted as well).
var projectPattern = regexp.MustCompile(`^([0-9a-f]{32}|[0-9a-f]{40})$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CanonicalSessionID returns signal unchanged when it already carries an
// official session shape (preserving upstream prompt-cache affinity).
// Any other identity is deterministically hashed into the canonical shape
// so the same conversation always maps to the same session.
// NOTE (v2.0.29): hashed sessions carry a random (non-time-ordered) time
// part and are rejected by the tightened gate, which checks the embedded
// timestamp for freshness. FreshSessionID below must be used for any
// session minted for upstream; CanonicalSessionID is only for validating
// (preserving) genuine client-supplied sessions.
func CanonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := hex.EncodeToString(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// sessionClock replicates packages/schema/src/identifier.ts create(true):
// per-ms counter so same-ms requests don't collide. The first 12 hex
// chars are time-ordered descending data (~(timestamp*0x1000+counter)).
// Measured against the live v2.0.29 gate: the embedded counter must stay
// tiny (1..32 pass, 64+ is rejected with 403), so a same-ms burst waits
// for the next millisecond instead of minting a doomed session.
var sessionClock struct {
	sync.Mutex
	last int64
	ctr  uint64
}

// maxSessionCounter caps the per-millisecond counter: live-bisected
// against the v2.0.29 free-tier gate (32 passes, 64 fails).
const maxSessionCounter = 8

// FreshSessionID mints a genuine time-ordered descending session ID,
// indistinguishable from what opencode 2.0.29 generates locally.
func FreshSessionID() string {
	for {
		now := time.Now().UnixMilli()
		sessionClock.Lock()
		if now != sessionClock.last {
			sessionClock.last = now
			sessionClock.ctr = 0
		}
		if sessionClock.ctr < maxSessionCounter {
			sessionClock.ctr++
			ctr := sessionClock.ctr
			sessionClock.Unlock()
			return mintSessionID(now, ctr)
		}
		sessionClock.Unlock()
		time.Sleep(time.Millisecond)
	}
}

func mintSessionID(now int64, ctr uint64) string {
	current := uint64(now)*0x1000 + ctr
	value := ^current
	var timeBytes [6]byte
	for i := 0; i < 6; i++ {
		shift := uint((5 - i) * 8)
		timeBytes[i] = byte((value >> shift) & 0xff)
	}
	timePart := hex.EncodeToString(timeBytes[:])
	randomPart := base62Random(14)
	return "ses_" + timePart + randomPart
}

// sessionCounter extracts the embedded per-ms counter from a canonical
// session id: counter = ~(low 12 bits of the time part).
func sessionCounter(id string) (uint64, bool) {
	if !canonicalSessionPattern.MatchString(id) {
		return 0, false
	}
	var v uint64
	for _, c := range id[4:16] {
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		default:
			return 0, false
		}
		v = v*16 + d
	}
	return (^v) & 0xFFF, true
}

// storedSessionUsable reports whether a hash-store session is still safe
// to send upstream. Legacy hash-minted sessions carry a random time part
// (counter almost surely large) and the v2.0.29 gate rejects them, so
// only sessions with a tiny embedded counter are reused.
func storedSessionUsable(id string) bool {
	ctr, ok := sessionCounter(id)
	return ok && ctr >= 1 && ctr <= maxSessionCounter
}

// base62Random returns n random base62 chars from crypto/rand.
func base62Random(n int) string {
	const chars = base62Alphabet
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = chars[int(b)%62]
	}
	return string(out)
}

// StableID derives a stable prefixed hex ID from a signal.
func StableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

// CanonicalProjectID returns signal unchanged when it already carries
// a genuine project shape: 40 hex (sha1 of the directory or git identity,
// what real clients send), 32 hex (accepted), or the literal "global"
// (Project.ID.global, the fallback real clients send when no identity
// resolves). Every other shape — legacy prj_<24hex>, names — is hashed
// with sha1 into 40 hex, mirroring Hash.fast in packages/util/src/hash.ts,
// so the same conversation always maps to the same project and no legacy
// shape reaches the v2 gate.
func CanonicalProjectID(signal string) string {
	s := strings.TrimSpace(signal)
	if projectPattern.MatchString(s) || s == "global" {
		return s
	}
	sum := sha1.Sum([]byte("prj\x00" + s))
	return hex.EncodeToString(sum[:])
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
