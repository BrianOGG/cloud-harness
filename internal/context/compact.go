package context

import (
	"encoding/json"
	"strings"
)

// ToolRef is the id pairing a call with its result. Compaction keeps both.
type ToolRef struct {
	ID        string          `json:"id"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CompactOptions selects what a visible context view may change.
type CompactOptions struct {
	PrefixCount int
	KeepIndex   int
	TokenLimit  int
}

// Compact shortens the dynamic suffix once it exceeds TokenLimit.
// The prefix, the current user message, and the newest tool-call pair stay verbatim.
// Older tool results keep their tool_call_id. The function does not receive opaque items.
func Compact(msgs []Message, opt CompactOptions) []Message {
	if opt.TokenLimit <= 0 || Estimate(msgs) <= opt.TokenLimit {
		return msgs
	}
	out := cloneMessages(msgs)
	lastPair := -1
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Role == "assistant" && len(out[i].ToolCalls) > 0 {
			lastPair = i
			break
		}
	}
	for i := range out {
		if i < opt.PrefixCount || i == opt.KeepIndex {
			continue
		}
		if lastPair >= 0 && i >= lastPair {
			continue
		}
		if out[i].Role == "tool" {
			out[i].Content = compactTool(out[i])
			continue
		}
		if len([]rune(out[i].Content)) > 160 {
			out[i].Content = clip(out[i].Content, 160) + "\n[compacted; raw history retained]"
		}
	}
	return out
}

// ToolPairsIntact reports whether every tool call id has one matching result and the reverse.
func ToolPairsIntact(msgs []Message) bool {
	calls := map[string]int{}
	results := map[string]int{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.ID == "" {
					return false
				}
				calls[c.ID]++
			}
		}
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return false
			}
			results[m.ToolCallID]++
		}
	}
	if len(calls) != len(results) {
		return false
	}
	for id, n := range calls {
		if n != 1 || results[id] != 1 {
			return false
		}
	}
	return true
}

func compactTool(m Message) string {
	excerpt := clip(m.Content, 80)
	return `{"untrusted":true,"compacted":true,"tool_call_id":"` + jsonEscape(m.ToolCallID) + `","excerpt":"` + jsonEscape(excerpt) + `","ref":"` + jsonEscape(m.ToolCallID) + `"}`
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func cloneMessages(in []Message) []Message {
	out := make([]Message, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolRef(nil), m.ToolCalls...)
		}
	}
	return out
}

// Estimate is a stable stand-in for provider token counts.
func Estimate(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += (len(m.Content)+3)/4 + 4
	}
	if n < 1 {
		return 1
	}
	return n
}
