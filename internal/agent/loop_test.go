package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func testManager(t *testing.T, mock *provider.Mock) (*Manager, *sqlite.DB, string) {
	t.Helper()
	root := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, root); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: db, Provider: mock, Broker: broker, Workspace: root, OwnerID: "loop"}, db, root
}

func toolResp(calls ...domain.ToolCall) domain.ModelResponse {
	items := make([]domain.ResponseItem, len(calls))
	for i, c := range calls {
		c.Ordinal = i
		items[i] = domain.ResponseItem{Ordinal: i, Kind: domain.ItemToolCall, ToolCall: &c}
	}
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: items}
}

func TestThreeReadsExecuteInOrderOnce(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(
			domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"alpha.txt"}`)},
			domain.ToolCall{ProviderCallID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"beta.txt"}`)},
			domain.ToolCall{ProviderCallID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"path":"gamma.txt"}`)},
		),
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "three files read"}}},
	}}
	m, db, root := testManager(t, mock)
	for name, body := range map[string]string{"alpha.txt": "A", "beta.txt": "B", "gamma.txt": "C"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "read three")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "three files read" {
		t.Fatalf("%+v", run)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 3 {
		t.Fatalf("batches %#v %v", batches, err)
	}
	want := []string{"c1", "c2", "c3"}
	for i, inv := range batches[0].Invocations {
		if inv.ProviderCallID != want[i] || inv.Status != domain.InvSucceeded || inv.Result == nil || !inv.Result.OK {
			t.Fatalf("inv %d %+v", i, inv)
		}
		if !strings.Contains(string(inv.Result.Data), string(rune('A'+i))) && i == 0 && !strings.Contains(string(inv.Result.Data), "A") {
			t.Fatalf("result %s", inv.Result.Data)
		}
	}
	if strings.Count(string(must(t, batches)), `"provider_call_id":"c1"`) != 1 {
		t.Fatal("first call id was duplicated")
	}
}

func TestMixedControlHasNoSideEffects(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "f", Name: "finish_turn", Arguments: json.RawMessage(`{"final_answer":"no"}`)}},
			{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "r", Name: "write_file", Arguments: json.RawMessage(`{"path":"pwn.txt","content":"x"}`)}},
		},
	}, {Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "corrected"}}}}}
	m, _, root := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "mix")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("%+v", run)
	}
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatal("mixed finish_turn wrote a file")
	}
}

func TestFinishTurnAndTextSummaryPaths(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "f1", Name: "finish_turn",
			Arguments: json.RawMessage(`{"final_answer":"ship it","turn_summary":"decided to ship","memory_delta":[{"op":"ADD","fact":"ship"}],"source_refs":["e1"]}`),
		}}},
	}}}
	m, db, _ := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "finish")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "ship it" {
		t.Fatalf("%+v", run)
	}
	summary, err := db.GetTurnSummary(ctx, run.TurnID)
	if err != nil || summary != "decided to ship" {
		t.Fatalf("summary %q %v", summary, err)
	}
	if run.Budget.UsedModelCalls != 1 {
		t.Fatalf("finish_turn called the model again: %+v", run.Budget)
	}
}

func TestDangerousCommandWaitsWithoutRunning(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(
		domain.ToolCall{ProviderCallID: "d1", Name: "run_command", Arguments: json.RawMessage(`{"command":"rm -rf ."}`)},
	)}}
	m, db, root := testManager(t, mock)
	sentinel := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("%+v", run)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "safe" {
		t.Fatal("dangerous command ran")
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || batches[0].Invocations[0].Status != domain.InvWaitingApproval || batches[0].Invocations[0].Dispatched {
		t.Fatalf("batch %#v %v", batches, err)
	}
}

func TestToolDefsExposeControlTools(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "d1",
			Name:           domain.ToolDelegateTask,
			Arguments:      json.RawMessage(`{"goal":"finish child with structured summary","max_model_calls":2,"max_tool_calls":2}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "cf1",
			Name:           domain.ToolFinishTurn,
			Arguments:      json.RawMessage(`{"final_answer":"child done","turn_summary":"child finished"}`),
		}),
		textOnly("parent done"),
	}}
	m, db, root := testManager(t, mock)

	topByName := map[string]provider.ToolDefinition{}
	for _, td := range m.toolDefs() {
		topByName[td.Name] = td
	}
	for _, want := range []string{domain.ToolFinishTurn, domain.ToolAskUser, domain.ToolDelegateTask} {
		if _, ok := topByName[want]; !ok {
			t.Fatalf("top-level toolDefs missing %q: %+v", want, topByName)
		}
	}

	var finishSchema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(topByName[domain.ToolFinishTurn].Parameters, &finishSchema); err != nil {
		t.Fatalf("unmarshal finish_turn schema: %v", err)
	}
	if finishSchema.Type != "object" {
		t.Fatalf("finish_turn schema type=%q, want object", finishSchema.Type)
	}
	for _, prop := range []string{"final_answer", "turn_summary", "memory_delta", "source_refs"} {
		if _, ok := finishSchema.Properties[prop]; !ok {
			t.Fatalf("finish_turn schema missing property %q: %s", prop, topByName[domain.ToolFinishTurn].Parameters)
		}
	}
	var memDeltaProp struct {
		Type  string `json:"type"`
		Items struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		} `json:"items"`
	}
	if err := json.Unmarshal(finishSchema.Properties["memory_delta"], &memDeltaProp); err != nil {
		t.Fatalf("unmarshal memory_delta property: %v", err)
	}
	if memDeltaProp.Type != "array" || memDeltaProp.Items.Type != "object" {
		t.Fatalf("unexpected memory_delta schema: %+v", memDeltaProp)
	}
	for _, deltaField := range []string{"op", "section", "id", "text", "fact", "reason", "source"} {
		if _, ok := memDeltaProp.Items.Properties[deltaField]; !ok {
			t.Fatalf("memory_delta.items missing property %q", deltaField)
		}
	}

	var askSchema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(topByName[domain.ToolAskUser].Parameters, &askSchema); err != nil {
		t.Fatalf("unmarshal ask_user schema: %v", err)
	}
	if askSchema.Type != "object" {
		t.Fatalf("ask_user schema type=%q, want object", askSchema.Type)
	}
	if _, ok := askSchema.Properties["question"]; !ok {
		t.Fatalf("ask_user schema missing question property: %s", topByName[domain.ToolAskUser].Parameters)
	}
	hasQuestionReq := false
	for _, r := range askSchema.Required {
		if r == "question" {
			hasQuestionReq = true
		}
	}
	if !hasQuestionReq {
		t.Fatalf("ask_user required=%v, want question", askSchema.Required)
	}

	childMgr := &Manager{Store: db, Provider: mock, Broker: m.Broker, Workspace: root, Depth: 1}
	childByName := map[string]provider.ToolDefinition{}
	for _, td := range childMgr.toolDefs() {
		childByName[td.Name] = td
	}
	if _, ok := childByName[domain.ToolFinishTurn]; !ok {
		t.Fatalf("child toolDefs missing finish_turn: %+v", childByName)
	}
	if _, ok := childByName[domain.ToolAskUser]; ok {
		t.Fatalf("child toolDefs must not include ask_user: %+v", childByName)
	}
	if _, ok := childByName[domain.ToolDelegateTask]; ok {
		t.Fatalf("child toolDefs must not include delegate_task: %+v", childByName)
	}

	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "delegate and finish")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent done" {
		t.Fatalf("run %+v %v", run, err)
	}
	if len(mock.Requests) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(mock.Requests))
	}
	parentReqTools := map[string]bool{}
	for _, td := range mock.Requests[0].Tools {
		parentReqTools[td.Name] = true
	}
	if !parentReqTools[domain.ToolFinishTurn] || !parentReqTools[domain.ToolAskUser] || !parentReqTools[domain.ToolDelegateTask] {
		t.Fatalf("parent request tools missing control/delegate tools: %+v", parentReqTools)
	}
	childReqTools := map[string]bool{}
	for _, td := range mock.Requests[1].Tools {
		childReqTools[td.Name] = true
	}
	if !childReqTools[domain.ToolFinishTurn] {
		t.Fatalf("child request tools missing finish_turn: %+v", childReqTools)
	}
	if childReqTools[domain.ToolAskUser] || childReqTools[domain.ToolDelegateTask] {
		t.Fatalf("child request tools leaked ask_user or delegate_task: %+v", childReqTools)
	}
}

func TestFinishTurnPersistsMemoryDelta(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "ft_1",
			Name:           domain.ToolFinishTurn,
			Arguments: json.RawMessage(`{
				"final_answer":"database port set to 5432",
				"turn_summary":"configured postgres on 5432",
				"memory_delta":[
					{"op":"ADD","section":"facts","id":"db_port","text":"postgres runs on port 5432","source":"turn_1"},
					{"op":"ADD","section":"decisions","id":"db_engine","text":"use postgres","reason":"ACID compliance","source":"turn_1"}
				],
				"source_refs":["cfg_1"]
			}`),
		}),
	}}
	m, db, _ := testManager(t, mock)
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "configure database")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "database port set to 5432" {
		t.Fatalf("unexpected run state: %+v", run)
	}
	if run.Budget.UsedToolCalls != 0 {
		t.Fatalf("finish_turn must not execute via Broker.Invoke, used_tool_calls=%d", run.Budget.UsedToolCalls)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches %+v %v", batches, err)
	}
	if batches[0].Invocations[0].Dispatched || batches[0].Invocations[0].Status != domain.InvSucceeded {
		t.Fatalf("finish_turn invocation %+v", batches[0].Invocations[0])
	}
	summary, err := db.GetTurnSummary(ctx, run.TurnID)
	if err != nil || summary != "configured postgres on 5432" {
		t.Fatalf("summary=%q err=%v", summary, err)
	}
	rawMem, ver, err := db.LatestMemory(ctx, ses.ID)
	if err != nil || ver < 1 {
		t.Fatalf("LatestMemory ver=%d err=%v", ver, err)
	}
	mem, err := contextpkg.ParseMemory(rawMem)
	if err != nil {
		t.Fatalf("ParseMemory: %v", err)
	}
	if len(mem.Facts) != 1 || mem.Facts[0].ID != "db_port" || mem.Facts[0].Text != "postgres runs on port 5432" {
		t.Fatalf("unexpected facts in session memory: %+v", mem.Facts)
	}
	if len(mem.Decisions) != 1 || mem.Decisions[0].ID != "db_engine" || mem.Decisions[0].Text != "use postgres" || mem.Decisions[0].Reason != "ACID compliance" {
		t.Fatalf("unexpected decisions in session memory: %+v", mem.Decisions)
	}
}

func must(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnsureToolsRegistersFileToolsetAndEditApproval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Do not call tools.RegisterLocal manually: verify ensureTools registers the toolset.
	broker := tools.NewBroker(db, db)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(
			domain.ToolCall{ProviderCallID: "lf1", Name: "list_files", Arguments: json.RawMessage(`{"pattern":"*.txt"}`)},
			domain.ToolCall{ProviderCallID: "gr1", Name: "grep", Arguments: json.RawMessage(`{"pattern":"OLD_VAL","literal":true}`)},
			domain.ToolCall{ProviderCallID: "ed1", Name: "edit_file", Arguments: json.RawMessage(`{"path":"cfg.txt","old_string":"OLD_VAL","new_string":"NEW_VAL"}`)},
		),
		textOnly("edited after approval"),
	}}
	m := &Manager{Store: db, Provider: mock, Broker: broker, Workspace: root, OwnerID: "loop"}
	if err := os.WriteFile(filepath.Join(root, "cfg.txt"), []byte("key=OLD_VAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defs := map[string]provider.ToolDefinition{}
	for _, td := range m.toolDefs() {
		defs[td.Name] = td
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_files", "grep", "git_status", "git_diff", "git_log", "run_command"} {
		if _, ok := defs[want]; !ok {
			t.Fatalf("ensureTools did not expose %q in toolDefs", want)
		}
	}

	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "update cfg.txt")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("expected edit_file to wait for approval, got %+v", run)
	}
	body, err := os.ReadFile(filepath.Join(root, "cfg.txt"))
	if err != nil || string(body) != "key=OLD_VAL\n" {
		t.Fatalf("edit_file modified file before approval: %q %v", body, err)
	}

	resumed, err := m.Resume(ctx, run.ID, ResumeInput{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != domain.RunCompleted || resumed.FinalAnswer != "edited after approval" {
		t.Fatalf("resumed %+v", resumed)
	}
	body, err = os.ReadFile(filepath.Join(root, "cfg.txt"))
	if err != nil || string(body) != "key=NEW_VAL\n" {
		t.Fatalf("cfg.txt after approved edit = %q %v", body, err)
	}
}

func TestProviderSchemasContainOnlyStandardKeys(t *testing.T) {
	mock := &provider.Mock{}
	m, _, _ := testManager(t, mock)
	m.Broker.Register(tools.Spec{
		Name:            "polluted_spec",
		Description:     "Tool with internal metadata in raw parameters",
		Readonly:        true,
		Idempotent:      true,
		SideEffectClass: "none",
		Parameters: json.RawMessage(`{
			"type":"object",
			"readOnly":true,
			"sideEffect":"none",
			"reversible":true,
			"properties":{
				"input":{"type":"string","readOnly":true,"sideEffect":"none","reversible":false}
			},
			"required":["input"],
			"additionalProperties":false
		}`),
	})

	standardKeys := map[string]bool{
		"type":                 true,
		"properties":           true,
		"required":             true,
		"items":                true,
		"description":          true,
		"enum":                 true,
		"additionalProperties": true,
		"default":              true,
		"minimum":              true,
		"maximum":              true,
		"minItems":             true,
		"maxItems":             true,
		"minLength":            true,
		"maxLength":            true,
		"pattern":              true,
		"format":               true,
		"title":                true,
		"anyOf":                true,
		"oneOf":                true,
		"allOf":                true,
		"not":                  true,
		"const":                true,
	}
	var checkNode func(toolName, loc string, node map[string]any)
	checkNode = func(toolName, loc string, node map[string]any) {
		t.Helper()
		for k := range node {
			if !standardKeys[k] {
				t.Fatalf("tool %s at %s contains non-standard schema key %q in %v", toolName, loc, k, node)
			}
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for propName, propVal := range props {
				propMap, ok := propVal.(map[string]any)
				if !ok {
					t.Fatalf("tool %s property %s.%s is not a schema object: %#v", toolName, loc, propName, propVal)
				}
				checkNode(toolName, loc+".properties."+propName, propMap)
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			checkNode(toolName, loc+".items", items)
		}
	}

	defs := m.toolDefs()
	if len(defs) == 0 {
		t.Fatal("toolDefs returned no tools")
	}
	for _, td := range defs {
		var node map[string]any
		if err := json.Unmarshal(td.Parameters, &node); err != nil {
			t.Fatalf("tool %s parameters invalid JSON: %v (%s)", td.Name, err, td.Parameters)
		}
		checkNode(td.Name, "$", node)
		for _, forbidden := range []string{"readOnly", "readonly", "sideEffect", "sideEffectClass", "reversible"} {
			if strings.Contains(string(td.Parameters), `"`+forbidden+`"`) {
				t.Fatalf("tool %s provider schema leaked %q: %s", td.Name, forbidden, td.Parameters)
			}
		}
	}
}

func TestUpdatePlanRejectsEmptyAndOversize(t *testing.T) {
	ctx := context.Background()
	oversizePlan := strings.Repeat("p", 32*1024+1)
	validPlan := "1. Inspect loop.go\n2. Validate update_plan input\n3. Run tests"
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "up_empty",
			Name:           domain.ToolUpdatePlan,
			Arguments:      json.RawMessage(`{"plan":""}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "up_space",
			Name:           domain.ToolUpdatePlan,
			Arguments:      json.RawMessage(`{"plan":"   \t\n  "}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "up_oversize",
			Name:           domain.ToolUpdatePlan,
			Arguments:      json.RawMessage(`{"plan":"` + oversizePlan + `"}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "up_ok",
			Name:           domain.ToolUpdatePlan,
			Arguments:      json.RawMessage(`{"plan":` + string(must(t, validPlan)) + `}`),
		}),
		textOnly("plan updated"),
	}}
	m, db, _ := testManager(t, mock)
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "validate update_plan")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "plan updated" {
		t.Fatalf("unexpected run state: %+v", run)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 4 {
		t.Fatalf("expected 4 batches, got %+v (err=%v)", batches, err)
	}
	for i, wantCallID := range []string{"up_empty", "up_space", "up_oversize"} {
		if len(batches[i].Invocations) != 1 {
			t.Fatalf("batch %d invocations: %+v", i, batches[i].Invocations)
		}
		inv := batches[i].Invocations[0]
		if inv.ProviderCallID != wantCallID {
			t.Fatalf("batch %d call_id=%q, want %q", i, inv.ProviderCallID, wantCallID)
		}
		if inv.Status != domain.InvFailed || inv.Result == nil || inv.Result.OK || inv.Result.Error == nil {
			t.Fatalf("batch %d (%s) expected failed invocation, got %+v", i, wantCallID, inv)
		}
		if inv.Result.Error.Code != domain.CodeInvalidArgument {
			t.Fatalf("batch %d (%s) error code=%q, want %q (err=%+v)", i, wantCallID, inv.Result.Error.Code, domain.CodeInvalidArgument, inv.Result.Error)
		}
	}

	okInv := batches[3].Invocations[0]
	if okInv.ProviderCallID != "up_ok" || okInv.Status != domain.InvSucceeded || okInv.Result == nil || !okInv.Result.OK {
		t.Fatalf("expected valid update_plan to succeed, got %+v", okInv)
	}
	var ack struct {
		Accepted bool    `json:"accepted"`
		Bytes    int     `json:"bytes"`
		Plan     *string `json:"plan"`
	}
	if err := json.Unmarshal(okInv.Result.Data, &ack); err != nil {
		t.Fatalf("unmarshal update_plan ack: %v (%s)", err, okInv.Result.Data)
	}
	if !ack.Accepted || ack.Bytes != len(validPlan) {
		t.Fatalf("unexpected update_plan ack: %+v (want accepted=true, bytes=%d)", ack, len(validPlan))
	}
	if ack.Plan != nil {
		t.Fatalf("update_plan ack echoed raw plan instead of structured ack: %s", okInv.Result.Data)
	}
}
