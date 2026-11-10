// Package context builds the model input as a stable prefix plus a dynamic suffix.
// M0 does not call a planner model. The split exists so later compaction can
// change the suffix without rewriting the prefix.
package context

import "encoding/json"

// Message is one chat message. Role is system, user, assistant, or tool.
// ToolCalls and ToolCallID keep a compacted view paired. They are omitted
// from the stable prefix, which is system text only.
type Message struct {
	Role       string    `json:"role"`
	Content    string    `json:"content"`
	ToolCalls  []ToolRef `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
}

// Assembly is the ordered model input.
type Assembly struct {
	StaticPrefix  []Message `json:"static_prefix"`
	DynamicSuffix []Message `json:"dynamic_suffix"`
}

// Assemble puts system instructions and the tool digest in the prefix.
// Memory, recent turns, and the current user message stay in the suffix.
func Assemble(system, toolDigest, memory string, recent []Message, user string) Assembly {
	prefix := []Message{{Role: "system", Content: system}}
	if toolDigest != "" {
		prefix = append(prefix, Message{Role: "system", Content: toolDigest})
	}
	var suffix []Message
	if memory != "" {
		suffix = append(suffix, Message{Role: "system", Content: memory})
	}
	suffix = append(suffix, recent...)
	suffix = append(suffix, Message{Role: "user", Content: user})
	return Assembly{StaticPrefix: prefix, DynamicSuffix: suffix}
}

// PrefixBytes is the canonical encoding of the stable prefix.
func PrefixBytes(a Assembly) ([]byte, error) {
	return json.Marshal(a.StaticPrefix)
}
