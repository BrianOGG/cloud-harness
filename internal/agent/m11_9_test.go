package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	contextpkg "github.com/cloud-harness/cloud-harness/internal/context"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func TestSkillCatalogIsProgressivelyDisclosed(t *testing.T) {
	isolateRuleConfig(t)
	ctx := t.Context()
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly("baseline")}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	writeAgentRule(t, filepath.Join(root, "AGENTS.md"), "Project rule before the skill list.")
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessage(ctx, session.ID, "baseline without skills"); err != nil {
		t.Fatal(err)
	}
	baseline := mock.Requests[0].Prefix
	if len(baseline) != 3 || !strings.Contains(baseline[2].Content, "Project rule before the skill list.") {
		t.Fatalf("no-skill prefix = %+v", baseline)
	}
	const skillCount = 15
	var names, files, markers []string
	var calls []domain.ToolCall
	for i := range skillCount {
		name := fmt.Sprintf("skill-%02d", i)
		marker := "PRIVATE-SKILL-BODY-" + name
		file := fmt.Sprintf("---\nname: %s\ndescription: Description for %s\n---\n\n%s\n", name, name, marker)
		writeAgentRule(t, filepath.Join(root, ".agents", "skills", name, "SKILL.md"), file)
		names, files, markers = append(names, name), append(files, file), append(markers, marker)
		calls = append(calls, domain.ToolCall{
			ProviderCallID: name, Name: tools.LoadSkillName, Arguments: json.RawMessage(fmt.Sprintf(`{"name":%q}`, name)),
		})
	}
	calls = append(calls, domain.ToolCall{ProviderCallID: "missing", Name: tools.LoadSkillName, Arguments: json.RawMessage(`{"name":"missing"}`)})
	mock.Responses = []domain.ModelResponse{toolResp(calls...), textOnly("skills inspected")}
	run, err := m.RunMessage(ctx, session.ID, "inspect the catalog and skills")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "skills inspected" || len(mock.Requests) != 3 {
		t.Fatalf("run=%+v err=%v requests=%d", run, err, len(mock.Requests))
	}
	first := mock.Requests[1]
	if len(first.Prefix) != len(baseline)+1 || !reflect.DeepEqual(first.Prefix[:len(baseline)], baseline) ||
		!reflect.DeepEqual(first.Messages[:len(first.Prefix)], first.Prefix) ||
		!strings.Contains(first.Messages[len(first.Prefix)].Content, "baseline") ||
		!reflect.DeepEqual(mock.Requests[2].Prefix, first.Prefix) {
		t.Fatalf("skill list changed the rule prefix or history placement: %+v", first.Messages)
	}
	lines := strings.Split(first.Prefix[len(baseline)].Content, "\n")
	if len(lines) != skillCount+1 || !strings.Contains(lines[0], tools.LoadSkillName) {
		t.Fatalf("skill list = %q", lines)
	}
	for i, name := range names {
		if want := fmt.Sprintf("- %s: Description for %s", name, name); lines[i+1] != want {
			t.Fatalf("skill row %d = %q, want %q", i, lines[i+1], want)
		}
		if strings.Contains(messageText(first.Messages), markers[i]) {
			t.Fatalf("body of %s appeared before load_skill", name)
		}
	}
	// The runtime estimates one token per four UTF-8 bytes, rounded up, plus
	// four per message. Charge the full extra static message against 1200.
	if extra := contextpkg.Estimate(toContext(first.Prefix)) - contextpkg.Estimate(toContext(baseline)); extra >= 1200 {
		t.Fatalf("skill list adds %d estimated tokens", extra)
	}
	if !offeredTool(first, tools.LoadSkillName) {
		t.Fatal("load_skill is missing from model tools")
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != len(calls) {
		t.Fatalf("batches=%+v err=%v", batches, err)
	}
	for i, inv := range batches[0].Invocations[:skillCount] {
		if inv.Status != domain.InvSucceeded || inv.Result == nil || !inv.Result.OK {
			t.Fatalf("load %s: %+v", names[i], inv)
		}
		var loaded contextpkg.LoadedSkill
		if err := json.Unmarshal(inv.Result.Data, &loaded); err != nil || loaded.Name != names[i] || loaded.Truncated || loaded.Content != files[i] {
			t.Fatalf("load %s: %+v err=%v", names[i], loaded, err)
		}
	}
	missing := batches[0].Invocations[skillCount]
	if missing.Status != domain.InvFailed || missing.Result == nil || missing.Result.Error == nil ||
		missing.Result.Error.Code != domain.CodeToolError || !strings.Contains(missing.Result.Error.Message, `skill "missing" not found`) {
		t.Fatalf("missing skill: %+v", missing)
	}
}

func TestPlanRunKeepsReadOnlyToolsAcrossResume(t *testing.T) {
	isolateRuleConfig(t)
	ctx := t.Context()
	write := snapshotWrite("write", "planned.txt", "written")
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(write),
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"Continue planning?"}`)}),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	allowSnapshotWrite(m, root)
	// Exercise side-effect metadata separately from the local write tools.
	m.Broker.Register(tools.Spec{Name: "snapshot_write", Readonly: true, SideEffectClass: "reversible"})
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(ctx, session.ID, "make a plan", RunOptions{Plan: true})
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	resumedProvider := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "poll", Name: domain.ToolPoll, Arguments: json.RawMessage(`{"target_tool":"run_command","target_args":{"command":"pwd"},"until":{},"max_attempts":1}`)}),
		toolResp(write), textOnly("planned"),
		toolResp(write), textOnly("written normally"),
	}}
	resumer := &Manager{Store: db, Provider: resumedProvider, Broker: m.Broker, Workspace: root, OwnerID: m.OwnerID, SkipPlanner: true, Grants: m.Grants}
	run, err = resumer.Resume(ctx, run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "planned" {
		t.Fatalf("resumed plan run=%+v err=%v", run, err)
	}
	blocked := []string{"write_file", "edit_file", "run_command", domain.ToolDelegateTask, domain.ToolExecuteProgram, domain.ToolUpdatePlan, "snapshot_write"}
	for _, request := range append(mock.Requests, resumedProvider.Requests...) {
		for _, name := range blocked {
			if offeredTool(request, name) {
				t.Fatalf("plan run offered %q", name)
			}
		}
		for _, name := range []string{"read_file", tools.LoadSkillName, domain.ToolPoll, domain.ToolFinishTurn} {
			if !offeredTool(request, name) {
				t.Fatalf("plan run lost %q", name)
			}
		}
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 4 {
		t.Fatalf("plan run batches=%+v err=%v", batches, err)
	}
	assertUnknown := func(index int, target string, dispatched bool) {
		t.Helper()
		inv := batches[index].Invocations[0]
		if inv.Dispatched != dispatched || inv.Result == nil || inv.Result.Error == nil ||
			inv.Result.Error.Code != domain.CodeToolError || inv.Result.Error.Message != "unknown tool "+target {
			t.Fatalf("plan invocation=%+v result=%+v", inv, inv.Result)
		}
	}
	assertUnknown(0, "write_file", false)
	assertUnknown(2, "run_command", true) // only the read-only poll wrapper ran
	assertUnknown(3, "write_file", false)
	if _, err := os.Stat(filepath.Join(root, "planned.txt")); !os.IsNotExist(err) {
		t.Fatalf("plan wrote a file: %v", err)
	}
	records, err := db.ListWorkspaceSnapshots(ctx, run.WorkspaceID)
	if err != nil || len(records) != 0 {
		t.Fatalf("plan wrote snapshots: %v, %v", records, err)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var planEvents int
	for _, event := range events {
		if event.Type == "plan.mode" {
			planEvents++
			if string(event.Payload) != `{"enabled":true}` {
				t.Fatalf("plan event payload=%s", event.Payload)
			}
		}
	}
	if planEvents != 1 {
		t.Fatalf("got %d plan.mode events, want one", planEvents)
	}
	ordinary, err := resumer.RunMessage(ctx, session.ID, "write normally")
	if err != nil || ordinary.Status != domain.RunCompleted || !offeredTool(resumedProvider.Requests[len(resumedProvider.Requests)-1], "write_file") {
		t.Fatalf("normal run inherited plan restrictions: %+v err=%v", ordinary, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "planned.txt")); err != nil || string(data) != "written" {
		t.Fatalf("normal run could not write: %q err=%v", data, err)
	}
}

func offeredTool(request provider.Request, name string) bool {
	for _, tool := range request.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
