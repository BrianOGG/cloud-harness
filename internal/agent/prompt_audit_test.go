package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

var promptAuditModes = []string{"ask", "safe", "workspace-write", "full-auto"}

var promptAuditIdentity = regexp.MustCompile(`(?i)claude|anthropic|openai|gpt|gemini|llama|qwen|deepseek|mistral|kimi|moonshot|zhipu|glm|stepfun|agnes`)

func assertPromptAuditNeutral(t *testing.T, label, text string) {
	t.Helper()
	if match := promptAuditIdentity.FindString(text); match != "" {
		t.Errorf("%s contains a provider/model identity %q", label, match)
	}
}

func promptAuditRequest(t *testing.T, mode string, plan bool) provider.Request {
	t.Helper()
	isolateRuleConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, _, _ := testManager(t, mock)
	m.SkipPlanner, m.DisableRepoMap, m.DisableSnapshots = true, true, true
	m.ModelRouting = &config.ModelRouting{}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A selected model belongs in routing metadata, never the system text.
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "Audit the system instructions.", RunOptions{ApprovalMode: mode, Plan: plan, Model: "gpt-prompt-audit-fixture"})
	if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 1 {
		t.Fatalf("run=%+v error=%v requests=%d", run, err, len(mock.Requests))
	}
	req := mock.Requests[0]
	if req.Model != "gpt-prompt-audit-fixture" {
		t.Fatalf("selected model was not routed: %q", req.Model)
	}
	return req
}

func promptAuditSystemText(req provider.Request) (string, int, int) {
	var messages []string
	bytes := 0
	for _, message := range req.Messages {
		if message.Role == "system" {
			messages = append(messages, message.Content)
			bytes += len(message.Content)
		}
	}
	return strings.Join(messages, "\n"), bytes, len(messages)
}

// Measure actual model-facing system content, excluding JSON framing and separators.
func TestPromptAuditSystemBytes(t *testing.T) {
	for _, mode := range promptAuditModes {
		t.Run(mode, func(t *testing.T) {
			_, bytes, count := promptAuditSystemText(promptAuditRequest(t, mode, false))
			if count != 2 {
				t.Fatalf("got %d system messages, want two", count)
			}
			t.Logf("mode=%s system_messages=%d system_bytes=%d", mode, count, bytes)
		})
	}
}

func TestPromptAuditBuiltInPromptsNeutral(t *testing.T) {
	prompts := map[string]string{
		"default":                defaultSystem,
		"plan":                   planToolDigest,
		"child digest":           childToolDigest,
		"child base":             childBaseSystem,
		"child":                  childSystemPrompt("", "", ""),
		"child with contract":    childSystemPrompt("Inspect the assigned task.", "Report concrete findings.", "Return JSON."),
		"work summary":           contextpkg.SummaryPrompt,
		"work summary prefix":    contextpkg.WorkSummaryPrefix,
		"session summary prefix": contextpkg.SessionSummaryPrefix,
		"memory":                 (contextpkg.Memory{}).Prompt(),
		"planner":                contextpkg.FormatPlannerPrompt("Find earlier decisions.", nil),
	}
	for _, mode := range promptAuditModes {
		prompts[mode] = toolDigest(mode)
		prompts[mode+" system"] = defaultSystemForMode(mode)
	}
	for name, prompt := range prompts {
		t.Run(name, func(t *testing.T) { assertPromptAuditNeutral(t, name, prompt) })
	}
	if toolDigest("") != toolDigest("ask") {
		t.Fatal("an omitted mode must use ask instructions")
	}
	if defaultSystemForMode("") != defaultSystemForMode("ask") {
		t.Fatal("an omitted mode must use ask command rules")
	}
}

func TestPromptAuditRequestMatchesApprovalMode(t *testing.T) {
	for _, mode := range promptAuditModes {
		t.Run(mode, func(t *testing.T) {
			req := promptAuditRequest(t, mode, false)
			text, _, count := promptAuditSystemText(req)
			if count != 2 || req.Messages[0].Content != defaultSystemForMode(mode) || req.Messages[1].Content != toolDigest(mode) {
				t.Fatalf("incorrect system instructions: %+v", req.Messages)
			}
			assertPromptAuditNeutral(t, mode, text)
			assertPromptAuditToolReferences(t, text, req.Tools)
			modePhrase := map[string]string{
				"ask":             "Read-only tools and safe commands run directly; write_file, edit_file, and other commands wait",
				"safe":            "auto-authorizes only read-only file and git tools; writes and commands wait",
				"workspace-write": "workspace-scoped write_file and edit_file calls run automatically; other commands wait",
				"full-auto":       "Calls that would otherwise prompt execute directly",
			}[mode]
			if !strings.Contains(text, modePhrase) {
				t.Fatalf("missing %s behavior %q", mode, modePhrase)
			}
			lower := strings.ToLower(text)
			if mode == "full-auto" {
				for _, forbidden := range []string{"wait for approval", "waiting", "require approval", "requires approval", "ask for approval", "approval required", "outside paths are rejected", "unsupported"} {
					if strings.Contains(lower, forbidden) {
						t.Errorf("full-auto contains waiting instruction %q", forbidden)
					}
				}
				if !strings.Contains(lower, "do not request approval") || !strings.Contains(lower, "do not repeat a denied call") {
					t.Fatal("full-auto lost its no-prompt or denial instructions")
				}
				if n := strings.Count(lower, "approval"); n != 1 {
					t.Fatalf("full-auto must mention approval only in its no-request instruction, got %d", n)
				}
			} else if !strings.Contains(lower, "wait for approval") || !strings.Contains(lower, "waiting call must not be repeated") {
				t.Fatal("approval mode lost its waiting instructions")
			}
			assertPromptAuditCommandRules(t, mode, text)
			for _, phrase := range []string{"text-only response ends the turn", "only tool call in its response", "do not know the underlying model"} {
				if !strings.Contains(text, phrase) {
					t.Errorf("missing instruction %q", phrase)
				}
			}
		})
	}
}

func assertPromptAuditCommandRules(t *testing.T, mode, text string) {
	t.Helper()
	phrases := []string{"cwd defaults to the workspace"}
	if mode == "full-auto" {
		phrases = append(phrases, "File tools (read/write/edit) stay inside the workspace", "Commands may use shell syntax (pipes, redirection, operators)", "absolute paths or cwd outside the workspace", "Harness control files are hidden")
		for _, forbidden := range []string{"without a shell", "outside paths are rejected", "unsupported"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("full-auto has obsolete command rule %q", forbidden)
			}
		}
	} else {
		phrases = append(phrases, "without a shell", "outside paths are rejected", "Pipes, redirection", "ls, pwd, and wc")
	}
	for _, phrase := range phrases {
		if !strings.Contains(text, phrase) {
			t.Errorf("%s missing command instruction %q", mode, phrase)
		}
	}
}

func TestPromptAuditResumeKeepsFrozenCommandRules(t *testing.T) {
	for _, mode := range promptAuditModes {
		t.Run(mode, func(t *testing.T) {
			isolateRuleConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "question", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)}),
				textOnly("done"),
			}}
			m, db, root := testManager(t, mock)
			m.SkipPlanner, m.DisableRepoMap, m.DisableSnapshots = true, true, true
			m.ModelRouting = &config.ModelRouting{}
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "inspect command rules", RunOptions{ApprovalMode: mode})
			if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
				t.Fatalf("initial run=%+v err=%v", run, err)
			}
			nextMode := "full-auto"
			if mode == "full-auto" {
				nextMode = "ask"
			}
			if err := db.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: nextMode}); err != nil {
				t.Fatal(err)
			}
			fresh := &Manager{Store: db, Provider: mock, Broker: m.Broker, Workspace: root, OwnerID: "prompt-audit-resume", DisableSnapshots: true, DisableRepoMap: true, ModelRouting: m.ModelRouting}
			run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
			if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 2 {
				t.Fatalf("resumed run=%+v err=%v requests=%d", run, err, len(mock.Requests))
			}
			req := mock.Requests[1]
			text, _, count := promptAuditSystemText(req)
			if count != 2 || req.Messages[0].Content != defaultSystemForMode(mode) || req.Messages[1].Content != toolDigest(mode) {
				t.Fatalf("resumed prompt used new preferences: %+v", req.Messages)
			}
			assertPromptAuditCommandRules(t, mode, text)
			if mode == "full-auto" && strings.Count(strings.ToLower(text), "approval") != 1 {
				t.Fatal("resumed full-auto repeated approval instructions")
			}
			snap, err := fresh.loadCheckpoint(t.Context(), run.ID)
			if err != nil || snap.ApprovalMode != mode {
				t.Fatalf("snapshot mode=%q err=%v", snap.ApprovalMode, err)
			}
		})
	}
}

func TestPromptAuditToolRegistryNeutral(t *testing.T) {
	isolateRuleConfig(t)
	m, _, _ := testManager(t, &provider.Mock{})
	definitions := m.toolDefs()
	if len(definitions) == 0 {
		t.Fatal("empty tool catalog")
	}
	for _, name := range []string{"read_file", "write_file", "edit_file", "list_dir", "list_files", "grep", "run_command", "git_status", "git_diff", "git_log", "load_skill", "retrieve_history", "retrieve_resource", "delegate_task", "ask_oracle", "update_plan", "execute_program", "poll_tool", "finish_turn", "ask_user"} {
		if !offeredTool(provider.Request{Tools: definitions}, name) {
			t.Errorf("missing registered tool %s", name)
		}
	}
	for _, definition := range definitions {
		t.Run(definition.Name, func(t *testing.T) {
			assertPromptAuditNeutral(t, "name", definition.Name)
			assertPromptAuditNeutral(t, "description", definition.Description)
			// Scan the complete schema, including nested parameter descriptions.
			assertPromptAuditNeutral(t, "parameters", string(definition.Parameters))
			var schema any
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatal(err)
			}
			assertPromptAuditSchemaNeutral(t, schema)
			switch definition.Name {
			case "write_file", "edit_file":
				if !strings.Contains(definition.Description, "Subject to the session's approval mode.") {
					t.Fatal("file-write description hardcodes authorization")
				}
			case "run_command":
				for _, phrase := range []string{"single command (default cwd: workspace root)", "Shell syntax and cwd outside the workspace are only available in full-auto mode", "outside-workspace path arguments", "session's approval mode"} {
					if !strings.Contains(definition.Description, phrase) {
						t.Errorf("command description missing %q", phrase)
					}
				}
				for _, forbidden := range []string{"without a shell", "cwd inside the workspace"} {
					if strings.Contains(definition.Description, forbidden) {
						t.Errorf("command description hardcodes %q", forbidden)
					}
				}
			}
		})
	}
}

// Inspect decoded strings too, so JSON escapes cannot hide an identity in a
// nested parameter description or enum.
func assertPromptAuditSchemaNeutral(t *testing.T, value any) {
	t.Helper()
	switch value := value.(type) {
	case string:
		assertPromptAuditNeutral(t, "decoded schema", value)
	case map[string]any:
		for key, child := range value {
			assertPromptAuditNeutral(t, "schema key", key)
			assertPromptAuditSchemaNeutral(t, child)
		}
	case []any:
		for _, child := range value {
			assertPromptAuditSchemaNeutral(t, child)
		}
	}
}

// Snake-case identifiers in behavioral prompts must resolve to an offered tool
// or an actual parameter field; this also catches misspelled tool names.
func assertPromptAuditToolReferences(t *testing.T, text string, definitions []provider.ToolDefinition) {
	t.Helper()
	known := map[string]bool{}
	var fields func(any)
	fields = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if properties, ok := value["properties"].(map[string]any); ok {
				for name := range properties {
					known[name] = true
				}
			}
			for _, child := range value {
				fields(child)
			}
		case []any:
			for _, child := range value {
				fields(child)
			}
		}
	}
	for _, definition := range definitions {
		known[definition.Name] = true
		var schema any
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		fields(schema)
	}
	for _, name := range regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`).FindAllString(text, -1) {
		if !known[name] {
			t.Errorf("prompt references unregistered tool or parameter %q", name)
		}
	}
}

func TestPromptAuditPlanUsesItsRegistry(t *testing.T) {
	for _, mode := range promptAuditModes {
		t.Run(mode, func(t *testing.T) {
			req := promptAuditRequest(t, mode, true)
			text, _, _ := promptAuditSystemText(req)
			if req.Messages[1].Content != planToolDigest || !strings.Contains(text, "read-only plan mode") {
				t.Fatal("plan instructions were replaced by the session mode")
			}
			assertPromptAuditNeutral(t, "plan request", text)
			assertPromptAuditToolReferences(t, text, req.Tools)
			for _, name := range []string{"write_file", "edit_file", "run_command", "execute_program", "delegate_task", "update_plan"} {
				if offeredTool(req, name) {
					t.Errorf("plan offered %s", name)
				}
			}
			for _, name := range []string{"read_file", "load_skill", "finish_turn", "ask_user"} {
				if !offeredTool(req, name) {
					t.Errorf("plan omitted %s", name)
				}
			}
		})
	}
}

func TestPromptAuditChildUsesItsRegistry(t *testing.T) {
	isolateRuleConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, _, _ := testManager(t, mock)
	m.SkipPlanner, m.DisableRepoMap, m.DisableSnapshots = true, true, true
	m.ModelRouting = &config.ModelRouting{}
	m.Depth, m.AllowedTools = 1, []string{"read_file"}
	m.System = childSystemPrompt("", "Report findings.", "Plain text.")
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(t.Context(), session.ID, "inspect"); err != nil {
		t.Fatal(err)
	}
	if len(mock.Requests) != 1 {
		t.Fatalf("requests=%d", len(mock.Requests))
	}
	req := mock.Requests[0]
	text, _, _ := promptAuditSystemText(req)
	assertPromptAuditNeutral(t, "child request", text)
	assertPromptAuditToolReferences(t, text, req.Tools)
	if req.Messages[1].Content != childToolDigest || offeredTool(req, "ask_user") || offeredTool(req, "delegate_task") {
		t.Fatal("child instructions contradict its tool registry")
	}
}

func TestPromptAuditAuxiliaryRequestsNeutral(t *testing.T) {
	isolateRuleConfig(t)
	ctx := t.Context()
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("done")}}
	m, _, _ := testManager(t, mock)
	m.SkipPlanner, m.DisableRepoMap, m.DisableSnapshots = true, true, true
	m.ModelRouting = &config.ModelRouting{}
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	var captured []provider.Request
	m.Summarizer = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		captured = append(captured, req)
		return textOnly(`{"turn_summary":"done","memory_delta":[],"confirmed_facts":["done"],"in_progress":[],"pending":[],"key_files_and_decisions":[]}`), nil
	})
	if _, _, _, err := m.summarize(ctx, run, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CompactSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	planner := &provider.Mock{Responses: []domain.ModelResponse{textOnly(`{"resources":[]}`)}}
	m.Provider = planner
	m.injectPlan(ctx, run, nil, "Find earlier decisions.")
	if len(captured) != 2 || len(planner.Requests) != 1 {
		t.Fatalf("summary requests=%d planner requests=%d", len(captured), len(planner.Requests))
	}
	captured = append(captured, planner.Requests[0], provider.OracleRequest("Inspect this decision."))
	for i, req := range captured {
		text, _, count := promptAuditSystemText(req)
		if count != 1 {
			t.Fatalf("request %d has %d system messages", i, count)
		}
		for _, message := range req.Messages {
			assertPromptAuditNeutral(t, fmt.Sprintf("auxiliary %d %s", i, message.Role), message.Content)
		}
		assertPromptAuditNeutral(t, fmt.Sprint(i), text)
	}
}

// Cover inline system templates too, without copying production strings into
// fixtures. Runtime request tests above exercise each memory/planner path.
func TestPromptAuditInlineSystemTemplatesNeutral(t *testing.T) {
	for _, file := range []string{"memory.go", "../context/summarize.go", "../provider/oracle.go"} {
		t.Run(file, func(t *testing.T) {
			source, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			ast.Inspect(source, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				assertPromptAuditNeutral(t, file, value)
				count++
				return true
			})
			if count == 0 {
				t.Fatal("no templates inspected")
			}
		})
	}
}
