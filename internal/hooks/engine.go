package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/program"
)

// Input is sent as JSON on stdin, or through broker.invoke("hook.input", {}).
// It is never recorded in hook events. Scripts have no other broker capability.
type Input struct {
	Point        Point           `json:"hook"`
	RunID        string          `json:"run_id"`
	SessionID    string          `json:"session_id"`
	InvocationID string          `json:"invocation_id,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Prompt       string          `json:"prompt,omitempty"`
	Answer       string          `json:"answer,omitempty"`
}

// Event contains only bounded, redacted output and execution metadata.
type Event struct {
	Point        Point  `json:"hook"`
	Action       int    `json:"action"`
	Type         string `json:"type"`
	InvocationID string `json:"invocation_id,omitempty"`
	ExitCode     int    `json:"exit_code"`
	Blocked      bool   `json:"blocked"`
	Warning      bool   `json:"warning"`
	TimedOut     bool   `json:"timed_out,omitempty"`
	Stdout       string `json:"stdout,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type Outcome struct {
	Blocked bool
	Message string
}

// Recorder is synchronous: execution cannot proceed without its audit event.
type Recorder func(context.Context, Event) error

type Engine struct{ Config *Config }

func New(cfg *Config) *Engine { return &Engine{Config: cfg} }

func (e *Engine) Execute(ctx context.Context, input Input, record Recorder) (Outcome, error) {
	if e == nil || e.Config == nil {
		return Outcome{}, nil
	}
	for i, action := range e.Config.Hooks[input.Point] {
		event := e.executeAction(ctx, input, action)
		event.Point, event.Action, event.Type = input.Point, i+1, action.Type
		event.InvocationID = input.InvocationID
		if record != nil {
			if err := record(ctx, event); err != nil {
				return Outcome{}, err
			}
		}
		if event.Blocked {
			return Outcome{Blocked: true, Message: event.Message()}, nil
		}
	}
	return Outcome{}, nil
}

func (e *Engine) executeAction(ctx context.Context, input Input, action Action) Event {
	timeout := DefaultTimeout
	if action.TimeoutMS > 0 {
		timeout = timeDurationMS(action.TimeoutMS)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	redact := newRedactor()
	var event Event
	if err := validateAction(e.Config.Workspace, action); err != nil {
		event.ExitCode, event.Stderr = 2, err.Error()
	} else if action.Type == "command" {
		shell, _ := shellPath(action.Shell)
		dir, err := workingDir(e.Config.Workspace, action.Cwd)
		if err != nil {
			event.ExitCode, event.Stderr = 2, err.Error()
		} else {
			inputJSON, _ := json.Marshal(input)
			event = runProcess(ctx, dir, commandEnv(), string(inputJSON), shell, "-c", action.Command)
		}
	} else {
		inputJSON, _ := json.Marshal(input)
		res, err := program.Execute(ctx, program.Request{Script: action.Script}, func(_ context.Context, name string, _ json.RawMessage) (bool, json.RawMessage, *domain.Error) {
			if name == "hook.input" {
				return true, inputJSON, nil
			}
			return false, nil, domain.E(domain.CodeAuthDenied, "hooks may only invoke hook.input")
		})
		switch {
		case err != nil:
			event.ExitCode, event.Stderr = 1, err.Error()
		case !res.OK:
			event.ExitCode = 1
			if res.Error != nil {
				event.Stderr = res.Error.Error()
			}
		default:
			var reply struct {
				ExitCode *int   `json:"exit_code"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
			}
			if err := decode(res.Data, &reply); err != nil || reply.ExitCode == nil {
				event.ExitCode, event.Stderr = 1, "Starlark main() must return {exit_code: int, stdout?: string, stderr?: string}"
			} else {
				event.ExitCode, event.Stdout, event.Stderr = *reply.ExitCode, reply.Stdout, reply.Stderr
			}
		}
	}
	if ctx.Err() != nil {
		event.ExitCode, event.Blocked = 2, true
		event.TimedOut = ctx.Err() == context.DeadlineExceeded
		if event.TimedOut {
			event.Reason = fmt.Sprintf("hook timed out after %s", timeout)
		} else {
			event.Reason = "hook cancelled"
		}
	}
	event.Blocked = event.ExitCode == 2
	event.Warning = event.ExitCode != 0 && !event.Blocked
	if event.Blocked && event.Reason == "" {
		event.Reason = "hook exited with code 2"
	} else if event.Warning {
		event.Reason = fmt.Sprintf("hook exited with code %d; continuing", event.ExitCode)
	}
	event.Stdout, event.Stderr = safeOutput(redact, event.Stdout, event.Stderr)
	return event
}

func (event Event) Message() string {
	text := string(event.Point) + " hook: " + event.Reason
	for _, output := range []string{event.Stdout, event.Stderr} {
		if strings.TrimSpace(output) != "" {
			text += "\n" + strings.TrimSpace(output)
		}
	}
	return text
}

var (
	credentialAssignment = regexp.MustCompile(`(?i)((?:[a-z0-9_]*(?:api[_-]?key|secret|token|password|credential)[a-z0-9_]*|authorization)["']?\s*[:=]\s*["']?)([^\s"',;}]+)`)
	bearerCredential     = regexp.MustCompile(`(?i)(\bbearer\s+)[a-z0-9._~+/=-]+`)
	urlCredential        = regexp.MustCompile(`(://)[^/@\s]+:[^/@\s]+@`)
	privateKey           = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
)

// Remove known process credentials even when a hook prints them literally,
// plus common credential assignments, bearer headers and URL userinfo.
func newRedactor() func(string) string {
	var secrets []string
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if value != "" && (strings.Contains(upper, "KEY") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "CREDENTIAL")) {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return func(text string) string {
		for _, secret := range secrets {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
		text = privateKey.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
		text = bearerCredential.ReplaceAllString(text, "${1}[REDACTED]")
		text = urlCredential.ReplaceAllString(text, "${1}[REDACTED]@")
		return credentialAssignment.ReplaceAllString(text, "${1}[REDACTED]")
	}
}

func safeOutput(redact func(string) string, stdout, stderr string) (string, string) {
	stdout = truncate(redact(stdout), MaxOutputBytes)
	stderr = truncate(redact(stderr), MaxOutputBytes-len(stdout))
	return stdout, stderr
}

func truncate(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text
	}
	if limit < 3 {
		return ""
	}
	text = text[:limit-3]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + "…"
}
