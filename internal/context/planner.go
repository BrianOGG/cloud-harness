package context

import (
	"encoding/json"
	"strings"
)

// PlanInput is the deterministic decision to call the lightweight planner.
type PlanInput struct {
	PriorTurns   int
	UserText     string
	SuffixTokens int
	TokenLimit   int
	Force        bool
	Skip         bool
}

// Resource is one history row the planner wants in the suffix.
type Resource struct {
	ID        string `json:"id"`
	Precision string `json:"precision"`
}

// CatalogEntry is a lightweight summary of one session history document or turn summary.
type CatalogEntry struct {
	SourceID  string `json:"source_id"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	Summary   string `json:"summary"`
}

// FormatPlannerPrompt builds the planner user message with the session's history catalog.
func FormatPlannerPrompt(userText string, catalog []CatalogEntry) string {
	if len(catalog) == 0 {
		return userText
	}
	clipped := make([]CatalogEntry, len(catalog))
	for i, c := range catalog {
		c.Summary = clip(strings.TrimSpace(c.Summary), 80)
		clipped[i] = c
	}
	raw, err := json.Marshal(clipped)
	if err != nil {
		return userText
	}
	return userText + "\n\nAvailable history catalog:\n" + string(raw)
}

// ShouldPlan reports whether this turn should call the planner.
// A short session with a clear request and room in the context budget skips it.
func ShouldPlan(in PlanInput) (bool, string) {
	if in.Skip {
		return false, "skipped"
	}
	if in.Force {
		return true, "forced"
	}
	if hasCue(in.UserText) {
		return true, "cross-turn reference"
	}
	if in.PriorTurns >= 3 {
		return true, "long session"
	}
	if in.TokenLimit > 0 && in.SuffixTokens > in.TokenLimit {
		return true, "context budget"
	}
	return false, "short session"
}

func hasCue(text string) bool {
	lower := strings.ToLower(text)
	for _, cue := range []string{
		"earlier", "previous turn", "last time", "before that",
		"之前", "上次", "上回",
	} {
		if strings.Contains(lower, cue) {
			return true
		}
	}
	for _, prefix := range []string{"evt_", "trn_", "note_", "hist_", "ans_", "sum_"} {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// ParsePlan reads a planner JSON object. Surrounding prose is ignored.
func ParsePlan(text string) ([]Resource, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, errDelta("planner response has no JSON object")
	}
	var body struct {
		Resources []Resource `json:"resources"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &body); err != nil {
		return nil, err
	}
	var out []Resource
	for _, r := range body.Resources {
		if strings.TrimSpace(r.ID) == "" {
			continue
		}
		switch r.Precision {
		case "summary", "excerpt", "verbatim":
		default:
			r.Precision = "excerpt"
		}
		out = append(out, r)
	}
	return out, nil
}

// Present clips a raw history row to the requested precision.
// verbatim is the stored text. Compaction and presentation never rewrite opaque bytes;
// callers must not pass opaque payloads into raw.
func Present(raw, precision string) string {
	raw = strings.TrimSpace(raw)
	switch precision {
	case "summary":
		return clip(raw, 160)
	case "verbatim":
		return raw
	default:
		return clip(raw, 240)
	}
}

func clip(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
