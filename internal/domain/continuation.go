package domain

import (
	"bytes"
	"encoding/json"
)

// Replay returns a deep copy of items in the same order for the same provider.
// Opaque payload and signature bytes are unchanged.
func Replay(provider string, items []ResponseItem) ([]ResponseItem, error) {
	if provider == "" {
		return nil, E(CodeInvalidArgument, "replay requires a provider")
	}
	if err := ValidateItems(items); err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Kind == ItemOpaque && it.Opaque.Provider != provider {
			return nil, E(CodeProviderMismatch, "opaque item belongs to "+it.Opaque.Provider)
		}
	}
	return CloneItems(items), nil
}

// ContinuationCompatible rejects replay of opaque state onto a different provider.
func ContinuationCompatible(storedProvider, nextProvider string) error {
	if storedProvider == "" || nextProvider == "" {
		return E(CodeInvalidArgument, "provider name required")
	}
	if storedProvider != nextProvider {
		return E(CodeProviderMismatch, "opaque continuation cannot move from "+storedProvider+" to "+nextProvider)
	}
	return nil
}

// PrepareContinuation builds the items a follow-up request may send.
// The same provider receives every item, including opaque bytes, in order.
// A different provider receives text, tool calls, and tool results only.
// Opaque items are dropped and degraded is set; they are never rewritten as summaries.
func PrepareContinuation(targetProvider string, resp ModelResponse) (items []ResponseItem, degraded bool, err error) {
	if err := ValidateItems(resp.Items); err != nil {
		return nil, false, err
	}
	if resp.Provider == targetProvider {
		return CloneItems(resp.Items), false, nil
	}
	out := make([]ResponseItem, 0, len(resp.Items))
	for _, it := range resp.Items {
		if it.Kind == ItemOpaque {
			degraded = true
			continue
		}
		out = append(out, it)
	}
	cloned := CloneItems(out)
	for i := range cloned {
		cloned[i].Ordinal = i
	}
	return cloned, degraded, nil
}

// TraceView is the projection that may be shown or logged.
// Opaque items are removed. Usage and visible text stay.
func TraceView(items []ResponseItem) []ResponseItem {
	out := make([]ResponseItem, 0, len(items))
	for _, it := range items {
		if it.Kind == ItemOpaque {
			continue
		}
		out = append(out, it)
	}
	cloned := CloneItems(out)
	for i := range cloned {
		cloned[i].Ordinal = i
	}
	return cloned
}

// IndexableText is the only model text that may enter the history FTS index.
// Opaque payloads, signatures, and raw provider continuation bytes are excluded.
func IndexableText(items []ResponseItem) string {
	var b bytes.Buffer
	for _, it := range items {
		if it.Kind != ItemText {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(it.Text)
	}
	return b.String()
}

// OpaqueBlob is the restricted persistence form: ordered opaque items only.
type OpaqueBlob struct {
	Provider string       `json:"provider"`
	Items    []OpaqueItem `json:"items"`
}

// SplitOpaque separates restricted continuation bytes from the visible item list.
// Visible opaque positions keep kind and ordinal but carry no payload.
type VisibleItem struct {
	Ordinal        int             `json:"ordinal"`
	Kind           ItemKind        `json:"kind"`
	Text           string          `json:"text,omitempty"`
	ToolCall       *ToolCall       `json:"tool_call,omitempty"`
	ToolResult     *ToolResultView `json:"tool_result,omitempty"`
	Usage          *Usage          `json:"usage,omitempty"`
	OpaqueRef      bool            `json:"opaque_ref,omitempty"`
	ProviderCallID string          `json:"provider_call_id,omitempty"`
}

// SplitForStorage returns visible rows plus the opaque blob. The blob is empty
// when the response has no opaque items.
func SplitForStorage(resp ModelResponse) (visible []VisibleItem, blob []byte, err error) {
	if err := ValidateItems(resp.Items); err != nil {
		return nil, nil, err
	}
	ob := OpaqueBlob{Provider: resp.Provider}
	for _, it := range resp.Items {
		row := VisibleItem{Ordinal: it.Ordinal, Kind: it.Kind, Text: it.Text, Usage: it.Usage}
		switch it.Kind {
		case ItemToolCall:
			cp := *it.ToolCall
			cp.Arguments = append(json.RawMessage(nil), it.ToolCall.Arguments...)
			row.ToolCall = &cp
			row.ProviderCallID = cp.ProviderCallID
		case ItemToolResult:
			cp := *it.ToolResult
			cp.Data = append(json.RawMessage(nil), it.ToolResult.Data...)
			row.ToolResult = &cp
			row.ProviderCallID = cp.ProviderCallID
		case ItemOpaque:
			row.Text = ""
			row.OpaqueRef = true
			cp := *it.Opaque
			cp.Payload = append([]byte(nil), it.Opaque.Payload...)
			cp.Signature = append([]byte(nil), it.Opaque.Signature...)
			ob.Items = append(ob.Items, cp)
		}
		visible = append(visible, row)
	}
	if len(ob.Items) == 0 {
		return visible, nil, nil
	}
	raw, err := json.Marshal(ob)
	if err != nil {
		return nil, nil, E(CodeProtocol, "opaque blob encode failed")
	}
	return visible, raw, nil
}

// MergeFromStorage restores response order from visible rows and the opaque blob.
func MergeFromStorage(provider string, visible []VisibleItem, blob []byte) ([]ResponseItem, error) {
	var ob OpaqueBlob
	if len(blob) > 0 {
		if err := json.Unmarshal(blob, &ob); err != nil {
			return nil, E(CodeProtocol, "opaque blob decode failed")
		}
	}
	nOpaque := 0
	items := make([]ResponseItem, len(visible))
	for i, row := range visible {
		items[i] = ResponseItem{Ordinal: row.Ordinal, Kind: row.Kind, Text: row.Text, Usage: row.Usage, ToolCall: row.ToolCall, ToolResult: row.ToolResult}
		if row.Kind == ItemOpaque {
			if nOpaque >= len(ob.Items) {
				return nil, E(CodeProtocol, "opaque ref has no stored payload")
			}
			cp := ob.Items[nOpaque]
			nOpaque++
			items[i].Opaque = &cp
			items[i].Text = ""
		}
	}
	if nOpaque != len(ob.Items) {
		return nil, E(CodeProtocol, "opaque blob does not match visible refs")
	}
	if _, err := Replay(provider, items); err != nil {
		return nil, err
	}
	return items, nil
}

// ContainsOpaqueBytes reports whether raw contains any opaque payload or signature.
func ContainsOpaqueBytes(raw []byte, items []ResponseItem) bool {
	for _, it := range items {
		if it.Kind != ItemOpaque || it.Opaque == nil {
			continue
		}
		if len(it.Opaque.Payload) > 0 && bytes.Contains(raw, it.Opaque.Payload) {
			return true
		}
		if len(it.Opaque.Signature) > 0 && bytes.Contains(raw, it.Opaque.Signature) {
			return true
		}
	}
	return false
}
