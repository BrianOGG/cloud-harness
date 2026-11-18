package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func TestVersionAndMockRun(t *testing.T) {
	var out, err bytes.Buffer
	if code := Main([]string{"version"}, &out, &err); code != 0 || !strings.Contains(out.String(), "0.5.0-m5") {
		t.Fatalf("version %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	db := t.TempDir() + "/h.db"
	code := Main([]string{"run", "--db", db, "--provider", "mock", "--message", "status"}, &out, &err)
	if code != 0 {
		t.Fatalf("run %d %s %s", code, out.String(), err.String())
	}
	if !strings.Contains(out.String(), "Mock answer: status") || !strings.Contains(out.String(), "COMPLETED") {
		t.Fatalf("output %s", out.String())
	}
}

func TestResumeFlagOrder(t *testing.T) {
	var out, err bytes.Buffer
	db := t.TempDir() + "/h.db"
	code := Main([]string{"runs", "resume", "run_missing", "--decision", "deny", "--db", db}, &out, &err)
	if code != 2 || !strings.Contains(err.String(), "run id required") {
		t.Fatalf("id-first %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	code = Main([]string{"runs", "resume", "--decision", "deny", "--db", db, "run_missing"}, &out, &err)
	if code == 2 || strings.Contains(err.String(), "run id required") {
		t.Fatalf("flags-first %d %s %s", code, out.String(), err.String())
	}
}

func TestExternalBindRefused(t *testing.T) {
	var out, err bytes.Buffer
	code := Main([]string{"serve", "--addr", "0.0.0.0:19421", "--db", t.TempDir() + "/h.db"}, &out, &err)
	if code == 0 || !strings.Contains(err.String(), "refusing") {
		t.Fatalf("serve %d %s %s", code, out.String(), err.String())
	}
	out.Reset()
	err.Reset()
	code = Main([]string{"serve", "--addr", "127.0.0.1:19421", "--http-token", "short", "--db", t.TempDir() + "/h.db"}, &out, &err)
	if code == 0 || !strings.Contains(err.String(), "16") {
		t.Fatalf("expected short token to fail: %d %s %s", code, out.String(), err.String())
	}
}

func TestRunsRecoverCLI(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/h.db"
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-10 * time.Minute)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run_exp_1", "run_exp_2"} {
		if err := db.CreateRun(ctx, storage.Run{
			ID: runID, SessionID: "ses_1", Status: domain.RunRunning,
			Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveBatch(ctx, storage.StoredBatch{
			ID: "bat_" + runID, RunID: runID, Status: domain.BatchOpen, Action: domain.ActionExecute,
			Invocations: []domain.Invocation{{
				ID: "inv_" + runID, Ordinal: 0, ProviderCallID: "c1", Name: "write_file",
				Arguments: json.RawMessage(`{"path":"out.txt","content":"x"}`), Class: domain.ActionExecute,
				Status: domain.InvRunning, Dispatched: true,
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireLease(ctx, runID, "dead-worker", now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	var out, errBuf bytes.Buffer
	if code := Main([]string{"runs", "recover", "run_exp_1", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("recover single %d %s %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), `"RECONCILIATION"`) {
		t.Fatalf("single recover output: %s", out.String())
	}

	out.Reset()
	errBuf.Reset()
	if code := Main([]string{"runs", "recover", "--all-expired", "--db", dbPath}, &out, &errBuf); code != 0 {
		t.Fatalf("recover all-expired %d %s %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "run_exp_2") || !strings.Contains(out.String(), `"RECONCILIATION"`) {
		t.Fatalf("all-expired recover output: %s", out.String())
	}
}

type scriptedProvider struct {
	step func(call int, req provider.Request) (domain.ModelResponse, error)
	call int
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Complete(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
	s.call++
	return s.step(s.call, req)
}

func newChatTestManager(t *testing.T, prov provider.Provider) (*agent.Manager, *sqlite.DB, string) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, ws); err != nil {
		t.Fatal(err)
	}
	return &agent.Manager{Store: db, Provider: prov, Broker: broker, Workspace: ws, OwnerID: "chat-cli"}, db, ws
}

func TestChatMultiTurnAskUser(t *testing.T) {
	ctx := context.Background()
	prov := &scriptedProvider{
		step: func(call int, req provider.Request) (domain.ModelResponse, error) {
			if call == 1 {
				return domain.ModelResponse{
					Provider: "mock", Complete: true,
					Items: []domain.ResponseItem{{
						Ordinal: 0, Kind: domain.ItemToolCall,
						ToolCall: &domain.ToolCall{
							ProviderCallID: "ask_1",
							Name:           domain.ToolAskUser,
							Arguments:      json.RawMessage(`{"question":"Which environment should I deploy to?"}`),
						},
					}},
				}, nil
			}
			supplied := ""
			for _, m := range req.Messages {
				if m.Role == "user" && strings.HasPrefix(m.Content, "User reply: ") {
					supplied = strings.TrimPrefix(m.Content, "User reply: ")
				}
			}
			return domain.ModelResponse{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemText,
					Text: "Final deployment target: " + supplied,
				}},
			}, nil
		},
	}
	mgr, db, _ := newChatTestManager(t, prov)

	var out, errBuf bytes.Buffer
	code := runChat(ctx, mgr, "ses_chat_ask", "deploy the service", strings.NewReader("staging-eu\n"), &out, &errBuf)
	if code != 0 {
		t.Fatalf("chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	output := out.String()
	if !strings.Contains(output, "Which environment should I deploy to?") {
		t.Fatalf("expected pending question in stdout, got:\n%s", output)
	}
	if !strings.Contains(output, "Final deployment target: staging-eu") {
		t.Fatalf("expected final answer reflecting supplied answer in stdout, got:\n%s", output)
	}
	runs, err := db.ListRuns(ctx, "ses_chat_ask")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %+v %v", runs, err)
	}
	if runs[0].Status != domain.RunCompleted || runs[0].FinalAnswer != "Final deployment target: staging-eu" {
		t.Fatalf("unexpected run state: %+v", runs[0])
	}

	// Verify stdin EOF while waiting exits gracefully without cancelling the run.
	eofProv := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{
			Ordinal: 0, Kind: domain.ItemToolCall,
			ToolCall: &domain.ToolCall{
				ProviderCallID: "ask_eof",
				Name:           domain.ToolAskUser,
				Arguments:      json.RawMessage(`{"question":"Still there?"}`),
			},
		}},
	}}}
	eofMgr, eofDB, _ := newChatTestManager(t, eofProv)
	out.Reset()
	errBuf.Reset()
	code = runChat(ctx, eofMgr, "ses_eof", "start and disconnect", strings.NewReader(""), &out, &errBuf)
	if code != 0 {
		t.Fatalf("chat EOF exited %d: out=%s err=%s", code, out.String(), errBuf.String())
	}
	eofRuns, err := eofDB.ListRuns(ctx, "ses_eof")
	if err != nil || len(eofRuns) != 1 {
		t.Fatalf("eof runs: %+v %v", eofRuns, err)
	}
	if eofRuns[0].Status != domain.RunWaiting || eofRuns[0].WaitReason != domain.WaitUser {
		t.Fatalf("expected run to remain WAITING(USER) on stdin EOF, got %+v", eofRuns[0])
	}
}

func TestChatApprovalPromptApproveAndDeny(t *testing.T) {
	ctx := context.Background()

	t.Run("approve", func(t *testing.T) {
		mock := &provider.Mock{Responses: []domain.ModelResponse{
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemToolCall,
					ToolCall: &domain.ToolCall{
						ProviderCallID: "w_approve",
						Name:           "write_file",
						Arguments:      json.RawMessage(`{"path":"approved.txt","content":"approved-side-effect"}`),
					},
				}},
			},
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "approved write completed"}},
			},
		}}
		mgr, db, ws := newChatTestManager(t, mock)
		var out, errBuf bytes.Buffer
		code := runChat(ctx, mgr, "ses_approve", "write approved.txt", strings.NewReader("Approve\n"), &out, &errBuf)
		if code != 0 {
			t.Fatalf("approve chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
		}
		output := out.String()
		if !strings.Contains(output, "write_file") || !strings.Contains(output, "approved.txt") {
			t.Fatalf("expected approval prompt with tool name, args summary, and resource in stdout, got:\n%s", output)
		}
		if !strings.Contains(output, "approved write completed") {
			t.Fatalf("expected final answer in stdout, got:\n%s", output)
		}
		data, err := os.ReadFile(filepath.Join(ws, "approved.txt"))
		if err != nil || string(data) != "approved-side-effect" {
			t.Fatalf("expected side effect on disk, got %q %v", data, err)
		}
		runs, err := db.ListRuns(ctx, "ses_approve")
		if err != nil || len(runs) != 1 || runs[0].Status != domain.RunCompleted {
			t.Fatalf("runs: %+v %v", runs, err)
		}
	})

	t.Run("deny", func(t *testing.T) {
		mock := &provider.Mock{Responses: []domain.ModelResponse{
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{
					Ordinal: 0, Kind: domain.ItemToolCall,
					ToolCall: &domain.ToolCall{
						ProviderCallID: "w_deny",
						Name:           "write_file",
						Arguments:      json.RawMessage(`{"path":"denied.txt","content":"must-not-write"}`),
					},
				}},
			},
			{
				Provider: "mock", Complete: true,
				Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "write was denied"}},
			},
		}}
		mgr, db, ws := newChatTestManager(t, mock)
		var out, errBuf bytes.Buffer
		code := runChat(ctx, mgr, "ses_deny", "write denied.txt", strings.NewReader("no\n"), &out, &errBuf)
		if code != 0 {
			t.Fatalf("deny chat exited %d: out=%s err=%s", code, out.String(), errBuf.String())
		}
		output := out.String()
		if !strings.Contains(output, "write_file") || !strings.Contains(output, "denied.txt") {
			t.Fatalf("expected approval prompt in stdout, got:\n%s", output)
		}
		if _, err := os.Stat(filepath.Join(ws, "denied.txt")); !os.IsNotExist(err) {
			t.Fatalf("denied write_file must not create file, err=%v", err)
		}
		runs, err := db.ListRuns(ctx, "ses_deny")
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs: %+v %v", runs, err)
		}
		batches, err := db.LoadBatches(ctx, runs[0].ID)
		if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
			t.Fatalf("batches: %+v %v", batches, err)
		}
		inv := batches[0].Invocations[0]
		if inv.Status != domain.InvDenied || inv.Dispatched {
			t.Fatalf("expected DENIED invocation without dispatch, got %+v", inv)
		}
	})
}
