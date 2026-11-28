package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	workspacesnapshot "github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

const rewindCheckpoint1 = `{"messages":[{"role":"user","content":"at S1"}],"prefix_n":1}`
const rewindCheckpoint2 = `{"messages":[{"role":"user","content":"after S1"}],"prefix_n":2}`

type rewindFixture struct {
	db        *sqlite.DB
	dbPath    string
	workspace string
	run       storage.Run
	snapper   *workspacesnapshot.Snapshotter
	target    storage.WorkspaceSnapshot
}

func newRewindFixture(t *testing.T) *rewindFixture {
	t.Helper()
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("CLOUD_HARNESS_HTTP_TOKEN", "")
	ctx := context.Background()
	f := &rewindFixture{dbPath: filepath.Join(t.TempDir(), "rewind.db"), workspace: t.TempDir()}
	var err error
	f.db, err = sqlite.Open(ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	workspaceID, err := workspacesnapshot.WorkspaceIDForPath(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, storage.Session{ID: "ses_rewind", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f.run = storage.Run{
		ID: "run_rewind", SessionID: "ses_rewind", WorkspaceID: workspaceID,
		Status: domain.RunCompleted, FinalAnswer: "later answer", Budget: domain.DefaultBudget(),
		Version: 3, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.db.CreateRun(ctx, f.run); err != nil {
		t.Fatal(err)
	}
	f.snapper, err = workspacesnapshot.Open(f.workspace, rewindSnapshotStore{RecordStore: f.db, events: f.db, runID: f.run.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "A", "at S1\n")
	f.appendEvents(t, 2)
	if err := f.db.SaveCheckpoint(ctx, f.run.ID, 1, []byte(rewindCheckpoint1), now); err != nil {
		t.Fatal(err)
	}
	f.target, err = f.snapper.Snapshot(ctx, "S1")
	if err != nil {
		t.Fatal(err)
	}
	f.write(t, "A", "after S1\n")
	f.write(t, "B", "added later\n")
	f.appendEvents(t, 2)
	if err := f.db.SaveCheckpoint(ctx, f.run.ID, 2, []byte(rewindCheckpoint2), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.snapper.Snapshot(ctx, "S2"); err != nil {
		t.Fatal(err)
	}
	// The preview must use the live workspace, including edits after S2.
	f.write(t, "A", "unsaved live edit\n")
	return f
}

func (f *rewindFixture) write(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.workspace, name), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

func (f *rewindFixture) appendEvents(t *testing.T, count int) {
	t.Helper()
	for range count {
		if _, err := f.db.AppendEvent(context.Background(), storage.Event{
			RunID: f.run.ID, Type: "test", Persistent: true, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *rewindFixture) invoke(t *testing.T, stdin io.Reader, extra ...string) (int, string, string) {
	t.Helper()
	args := []string{"rewind", f.run.ID, "--to", f.target.ID, "--db", f.dbPath, "--workspace", f.workspace}
	args = append(args, extra...)
	var out, errOut bytes.Buffer
	code := MainWithIO(args, stdin, &out, &errOut)
	t.Logf("%s\nexit=%d\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), code, out.String(), errOut.String())
	return code, out.String(), errOut.String()
}

func (f *rewindFixture) assertFiles(t *testing.T, rewound bool) {
	t.Helper()
	want := "unsaved live edit\n"
	if rewound {
		want = "at S1\n"
	}
	got, err := os.ReadFile(filepath.Join(f.workspace, "A"))
	if err != nil || string(got) != want {
		t.Fatalf("A=%q, err=%v, want %q", got, err, want)
	}
	got, err = os.ReadFile(filepath.Join(f.workspace, "B"))
	if rewound {
		if !os.IsNotExist(err) {
			t.Fatalf("B should have been deleted: %q, %v", got, err)
		}
	} else if err != nil || string(got) != "added later\n" {
		t.Fatalf("B=%q, err=%v", got, err)
	}
}

func (f *rewindFixture) assertChat(t *testing.T, rewound bool) {
	t.Helper()
	wantEvents, wantCheckpoint := 4, rewindCheckpoint2
	if rewound {
		wantEvents, wantCheckpoint = 2, rewindCheckpoint1
	}
	events, err := f.db.ListEvents(context.Background(), f.run.ID, 0)
	if err != nil || len(events) != wantEvents {
		t.Fatalf("events=%+v, err=%v, want %d", events, err, wantEvents)
	}
	for i, ev := range events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event seq %d at %d", ev.Seq, i)
		}
	}
	cp, err := f.db.LatestCheckpoint(context.Background(), f.run.ID)
	if err != nil || string(cp) != wantCheckpoint {
		t.Fatalf("checkpoint=%s, err=%v, want %s", cp, err, wantCheckpoint)
	}
	gotRun, err := f.db.GetRun(context.Background(), f.run.ID)
	if err != nil || !reflect.DeepEqual(gotRun, f.run) {
		t.Fatalf("run metadata changed: %+v, err=%v", gotRun, err)
	}
}

func (f *rewindFixture) records(t *testing.T) []storage.WorkspaceSnapshot {
	t.Helper()
	records, err := f.snapper.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestRewindModes(t *testing.T) {
	for _, mode := range []string{"code-only", "chat-only", "all", "default"} {
		t.Run(mode, func(t *testing.T) {
			f := newRewindFixture(t)
			flags := []string{"--yes"}
			if mode != "default" {
				flags = append(flags, "--"+mode)
			}
			code, out, stderr := f.invoke(t, strings.NewReader(""), flags...)
			if code != 0 {
				t.Fatalf("exit=%d: %s", code, stderr)
			}
			withCode, withChat := mode != "chat-only", mode != "code-only"
			f.assertFiles(t, withCode)
			f.assertChat(t, withChat)
			records := f.records(t)
			if withCode {
				if len(records) != 3 || !strings.HasPrefix(records[2].Label, "rewind-safety/"+f.run.ID+"/") {
					t.Fatalf("missing safety snapshot: %+v", records)
				}
				if records[2].EventSeq == nil || *records[2].EventSeq != 4 || !strings.Contains(out, "safety_snapshot="+records[2].ID) {
					t.Fatalf("safety snapshot output/seq: %+v, output=%s", records[2], out)
				}
				if !strings.Contains(out, "M A") || !strings.Contains(out, "D B") || !strings.Contains(out, "code_files=2") {
					t.Fatalf("missing current-to-target diff: %s", out)
				}
			} else if len(records) != 2 || !strings.Contains(out, "code_files=0") || !strings.Contains(out, "safety_snapshot=none") {
				t.Fatalf("chat-only created snapshots or reported code changes: %+v; %s", records, out)
			}
			if withChat && (!strings.Contains(out, "Chat events to delete: 2 (seq 3–4)") || !strings.Contains(out, "deleted_events=2")) {
				t.Fatalf("missing chat preview/summary: %s", out)
			}
			if withCode && strings.Index(out, "Code changes") > strings.Index(out, "Safety snapshot:") {
				t.Fatalf("safety snapshot happened before preview: %s", out)
			}
			if withCode {
				// Rewind the code to the printed safety id, including live edits
				// not present in S2. Chat stays at its post-rewind boundary.
				f.target = records[2]
				if code, _, errOut := f.invoke(t, nil, "--code-only", "--yes"); code != 0 {
					t.Fatalf("undo code rewind: %s", errOut)
				}
				f.assertFiles(t, false)
				f.assertChat(t, withChat)
			}
		})
	}
}

func TestRewindNonTTYRefusesEvenPipedYes(t *testing.T) {
	for _, mode := range []string{"--all", "--code-only", "--chat-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newRewindFixture(t)
			before := f.records(t)
			code, out, stderr := f.invoke(t, strings.NewReader("y\n"), mode)
			if code == 0 || !strings.Contains(stderr, "--yes") || strings.Contains(out, "Safety snapshot:") {
				t.Fatalf("non-TTY exit=%d, out=%s, stderr=%s", code, out, stderr)
			}
			f.assertFiles(t, false)
			f.assertChat(t, false)
			if got := f.records(t); !reflect.DeepEqual(got, before) {
				t.Fatalf("refusal changed snapshot records: %+v", got)
			}
		})
	}
}

func TestRewindValidationDoesNotMutate(t *testing.T) {
	for _, test := range []struct {
		name string
		want string
	}{
		{"missing run", "run_missing"},
		{"missing snapshot", "snap_missing"},
		{"workspace mismatch", "belongs to workspace"},
		{"wrong current workspace", "current workspace"},
		{"null event seq", "no event_seq"},
		{"null event seq all", "no event_seq"},
		{"no checkpoint", "checkpoint"},
		{"conflicting modes", "choose only one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRewindFixture(t)
			flags := []string{"--yes", "--chat-only"}
			switch test.name {
			case "missing run":
				f.run.ID = "run_missing"
			case "missing snapshot":
				f.target.ID = "snap_missing"
			case "workspace mismatch":
				other, err := f.db.CreateWorkspaceSnapshot(context.Background(), storage.WorkspaceSnapshot{WorkspaceID: "other", TreeOID: f.target.TreeOID})
				if err != nil {
					t.Fatal(err)
				}
				f.target = other
			case "wrong current workspace":
				flags = append(flags, "--workspace", t.TempDir())
			case "null event seq", "null event seq all":
				plain, err := workspacesnapshot.Open(f.workspace, f.db)
				if err != nil {
					t.Fatal(err)
				}
				f.target, err = plain.Snapshot(context.Background(), "no-seq")
				if err != nil {
					t.Fatal(err)
				}
				if test.name == "null event seq all" {
					flags = []string{"--yes", "--all"}
				}
			case "no checkpoint":
				seq := int64(0)
				f.target.ID = "snap_before_checkpoint"
				f.target.EventSeq = &seq
				if _, err := f.db.CreateWorkspaceSnapshot(context.Background(), f.target); err != nil {
					t.Fatal(err)
				}
			case "conflicting modes":
				flags = append(flags, "--code-only")
			}
			before := f.records(t)
			code, _, stderr := f.invoke(t, nil, flags...)
			if code == 0 || !strings.Contains(stderr, test.want) {
				t.Fatalf("exit=%d, stderr=%s, want %q", code, stderr, test.want)
			}
			f.run.ID = "run_rewind"
			f.assertFiles(t, false)
			f.assertChat(t, false)
			if got := f.records(t); !reflect.DeepEqual(got, before) {
				t.Fatalf("validation changed snapshot records: %+v", got)
			}
		})
	}
}

func TestRewindFlagOrderAndHelp(t *testing.T) {
	f := newRewindFixture(t)
	var out, stderr bytes.Buffer
	code := MainWithIO([]string{"rewind", "--chat-only", "--yes", "--db", f.dbPath, "--workspace", f.workspace, "--to", f.target.ID, f.run.ID}, nil, &out, &stderr)
	if code != 0 {
		t.Fatalf("flags-first exit=%d, %s", code, stderr.String())
	}
	f.assertChat(t, true)
	out.Reset()
	stderr.Reset()
	if code := MainWithIO([]string{"rewind", "--help"}, nil, &out, &stderr); code != 0 {
		t.Fatalf("help exit %d", code)
	}
	for _, text := range []string{"--all", "--code-only", "--chat-only", "--yes", "y/N", "rewind-safety/"} {
		if !strings.Contains(stderr.String(), text) {
			t.Fatalf("help missing %q: %s", text, stderr.String())
		}
	}
}

func TestRewindChatWithoutGit(t *testing.T) {
	f := newRewindFixture(t)
	t.Setenv("PATH", "")
	code, _, stderr := f.invoke(t, nil, "--chat-only", "--yes")
	if code != 0 {
		t.Fatalf("chat-only requires Git: %s", stderr)
	}
	f.assertFiles(t, false)
	f.assertChat(t, true)
}

func TestRewindCodeNullSequenceAllowed(t *testing.T) {
	f := newRewindFixture(t)
	target := f.target
	target.ID = "snap_null_seq"
	target.EventSeq = nil
	var err error
	f.target, err = f.db.CreateWorkspaceSnapshot(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.invoke(t, nil, "--code-only", "--yes"); code != 0 {
		t.Fatalf("code-only null sequence: %s", stderr)
	}
	f.assertFiles(t, true)
	f.assertChat(t, false)
}

func TestRewindRefusesActiveRunBeforeSafetySnapshot(t *testing.T) {
	f := newRewindFixture(t)
	now := time.Now().UTC()
	lease, err := f.db.AcquireLease(context.Background(), f.run.ID, "active-agent", now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.invoke(t, nil, "--all", "--yes"); code == 0 || !strings.Contains(stderr, "in use") {
		t.Fatalf("active run accepted: %s", stderr)
	}
	f.assertFiles(t, false)
	f.assertChat(t, false)
	if got := len(f.records(t)); got != 2 {
		t.Fatalf("active run created safety snapshot: %d", got)
	}
	if err := f.db.RequireLease(context.Background(), f.run.ID, "active-agent", lease.LeaseVersion, now); err != nil {
		t.Fatalf("rewind changed active lease: %v", err)
	}
}

func TestRewindAutomaticSnapshotHasCheckpoint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "automatic.db")
	cfg := config.Config{DBPath: dbPath, Provider: "mock"}
	mgr, db, err := openManager(ctx, cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mgr.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "write", Name: "write_file", Arguments: []byte(`{"path":"A","content":"new"}`),
		}}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "done"}}},
	}}
	mgr.Grants = []domain.Grant{{Tool: "write_file", Op: "call", ResourceScope: root, State: "active", Subject: "test"}}
	if err := os.WriteFile(filepath.Join(root, "A"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	session, err := mgr.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := mgr.RunMessage(ctx, session.ID, "write A")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v, err=%v", run, err)
	}
	records, err := db.ListWorkspaceSnapshots(ctx, run.WorkspaceID)
	if err != nil || len(records) != 4 {
		t.Fatalf("automatic snapshots=%+v, err=%v", records, err)
	}
	if records[0].EventSeq == nil || records[3].EventSeq == nil || records[1].EventSeq != nil || records[2].EventSeq != nil {
		t.Fatalf("unexpected chat rewind boundaries: %+v", records)
	}
	var out, stderr bytes.Buffer
	if code := MainWithIO([]string{"rewind", run.ID, "--to", records[0].ID, "--all", "--yes", "--db", dbPath, "--workspace", root}, nil, &out, &stderr); code != 0 {
		t.Fatalf("rewind automatic pre snapshot: %s; %s", out.String(), stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "A"))
	if err != nil || string(data) != "old" {
		t.Fatalf("A=%q, err=%v", data, err)
	}
	events, err := db.ListEvents(ctx, run.ID, *records[0].EventSeq)
	if err != nil || len(events) != 0 {
		t.Fatalf("events after boundary=%+v, err=%v", events, err)
	}
}

func TestReaderIsTTYRejectsNullDevice(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if readerIsTTY(file) || readerIsTTY(strings.NewReader("yes\n")) || readerIsTTY(nil) {
		t.Fatal("non-terminal input treated as a TTY")
	}
}
