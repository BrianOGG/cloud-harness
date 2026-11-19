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
	Ref        string    `json:"ref,omitempty"`
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

// WithProjectRules appends one frozen project-rules section after the static
// system instructions, before memory and history. Protect it as part of the
// stable prefix so compaction cannot rewrite rules during a run.
// Empty rules preserve the original assembly byte for byte.
func WithProjectRules(a Assembly, rules string) Assembly {
	if rules == "" {
		return a
	}
	prefix := append([]Message(nil), a.StaticPrefix...)
	a.StaticPrefix = append(prefix, Message{Role: "system", Content: rules})
	return a
}

// WithRepoMap places a workspace map at the start of the dynamic suffix. The
// caller can rebuild it for each model request without changing the stable
// prefix or persisting a stale map in a run checkpoint.
func WithRepoMap(a Assembly, repoMap string) Assembly {
	if repoMap == "" {
		return a
	}
	suffix := make([]Message, 0, len(a.DynamicSuffix)+1)
	suffix = append(suffix, Message{Role: "system", Content: "Repo-Map:\n" + repoMap})
	suffix = append(suffix, a.DynamicSuffix...)
	a.DynamicSuffix = suffix
	return a
}

// PrefixBytes is the canonical encoding of the stable prefix.
func PrefixBytes(a Assembly) ([]byte, error) {
	return json.Marshal(a.StaticPrefix)
}
