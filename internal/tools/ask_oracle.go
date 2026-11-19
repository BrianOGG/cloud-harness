package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const ToolAskOracle = "ask_oracle"

// OraclePrompt carries the prepared input and the sizes recorded in telemetry.
type OraclePrompt struct {
	Text           string
	QuestionLength int
	CodeBytes      int
}

type oracleKey struct{}

type oracleRun struct {
	runID string
	ask   func(context.Context, OraclePrompt) (string, error)
}

// WithOracle binds a consultation handler to a single run. The broker can be
// shared with other runs and with delegated agents.
func WithOracle(ctx context.Context, runID string, ask func(context.Context, OraclePrompt) (string, error)) context.Context {
	return context.WithValue(ctx, oracleKey{}, oracleRun{runID: runID, ask: ask})
}

func AskOracleSpec() Spec {
	return Spec{
		Name:            ToolAskOracle,
		Description:     "Consult an expert model on a difficult question. Returns advice as text. Include only relevant code (up to 8 KiB) and context (up to 4 KiB). Limited to three consultations per run.",
		Required:        []string{"question"},
		Readonly:        true,
		SideEffectClass: "none",
		EgressScope:     "provider",
		Parameters: json.RawMessage(`{"type":"object","properties":{
			"question":{"type":"string","description":"The specific question for the expert."},
			"code":{"type":"string","description":"Optional code snippet. Truncated at 8 KiB."},
			"context":{"type":"string","description":"Optional background. Truncated at 4 KiB."}
		},"required":["question"],"additionalProperties":false}`),
		Execute: executeAskOracle,
	}
}

func executeAskOracle(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	req, _ := ctx.Value(requestKey).(Request)
	oracle, ok := ctx.Value(oracleKey{}).(oracleRun)
	if !ok || req.RunID != oracle.runID || req.InvocationID == "" {
		return nil, domain.E(domain.CodeAuthDenied, "ask_oracle requires a direct call from the main run")
	}
	var input struct {
		Question string `json:"question"`
		Code     string `json:"code"`
		Context  string `json:"context"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(args)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || strings.TrimSpace(input.Question) == "" {
		return nil, domain.E(domain.CodeInvalidArgument, "ask_oracle requires a non-empty question and string code and context when supplied")
	}
	code := boundedOracleText(input.Code, 8<<10, "\n[code truncated at 8 KiB]")
	background := boundedOracleText(input.Context, 4<<10, "\n[context truncated at 4 KiB]")
	text := "Question:\n" + maskOracleText(input.Question)
	if code != "" {
		text += "\n\nCode snippet:\n" + code
	}
	if background != "" {
		text += "\n\nContext:\n" + background
	}
	answer, err := oracle.ask(ctx, OraclePrompt{
		Text: text, QuestionLength: utf8.RuneCountInString(input.Question), CodeBytes: len(code),
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"answer": answer})
}

func boundedOracleText(text string, limit int, marker string) string {
	masked := maskOracleText(text)
	if len(text) <= limit && len(masked) <= limit {
		return masked
	}
	n := min(len(masked), limit-len(marker))
	for n < len(masked) && !utf8.RuneStart(masked[n]) {
		n--
	}
	return masked[:n] + marker
}

// These text patterns follow the CLI's export --sanitize rules. Run them
// before truncation so a cutoff cannot expose the prefix of a credential.
var oracleMasks = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password|authorization)["']?\s*(?::=|[:=])\s*")([^"]*)(")`), `${1}[redacted]${3}`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password|authorization)["']?\s*(?::=|[:=])\s*')([^']*)(')`), `${1}[redacted]${3}`},
	{regexp.MustCompile(`(?im)(\bauthorization\s*:\s*)[^\r\n]+`), `${1}[redacted]`},
	{regexp.MustCompile(`(?i)((?:api[_-]?key|apikey|token|secret|password)["']?\s*(?::=|[:=])\s*)([^,\s;&}\]"']+)`), `${1}[redacted]`},
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*`), `Bearer [redacted]`},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`), `[redacted]`},
	{regexp.MustCompile(`(?i)hsurr:[^\s,;}\]\)"']+`), `[redacted]`},
}

func maskOracleText(text string) string {
	for _, mask := range oracleMasks {
		text = mask.pattern.ReplaceAllString(text, mask.replacement)
	}
	return text
}
