package domain

// PairToolResults checks that a follow-up request carries exactly one result
// for each accepted tool call id, with no extras.
func PairToolResults(calls []ToolCall, results []ToolResultView) error {
	if len(calls) != len(results) {
		return E(CodeProtocol, "tool result count does not match tool calls")
	}
	seen := map[string]struct{}{}
	for _, c := range calls {
		seen[c.ProviderCallID] = struct{}{}
	}
	for _, r := range results {
		if _, ok := seen[r.ProviderCallID]; !ok {
			return E(CodeProtocol, "tool result has no matching call id "+r.ProviderCallID)
		}
		delete(seen, r.ProviderCallID)
	}
	if len(seen) != 0 {
		return E(CodeProtocol, "tool call missing a result")
	}
	return nil
}
