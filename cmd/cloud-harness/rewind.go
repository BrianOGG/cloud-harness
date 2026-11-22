package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	workspacesnapshot "github.com/BrianOGG/cloud-harness/internal/snapshot"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

type rewindSnapshotStore struct {
	workspacesnapshot.RecordStore
	events storage.Store
	runID  string
}

func (s rewindSnapshotStore) CreateWorkspaceSnapshot(ctx context.Context, rec storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error) {
	events, err := s.events.ListEvents(ctx, s.runID, 0)
	if err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	for _, event := range events {
		if event.Persistent && event.Seq > 0 && (rec.EventSeq == nil || event.Seq > *rec.EventSeq) {
			seq := event.Seq
			rec.EventSeq = &seq
		}
	}
	return s.RecordStore.CreateWorkspaceSnapshot(ctx, rec)
}

func cmdRewind(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rewind", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := addCommon(fs)
	targetID := fs.String("to", "", "target workspace snapshot id")
	all := fs.Bool("all", false, "rewind both code and chat (default)")
	codeOnly := fs.Bool("code-only", false, "restore workspace files only")
	chatOnly := fs.Bool("chat-only", false, "rewind persistent events and checkpoint only")
	yes := fs.Bool("yes", false, "confirm without prompting")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: cloud-harness rewind RUN_ID --to SNAPSHOT_ID [--all|--code-only|--chat-only] [--yes] [--db path] [--workspace dir]")
		_, _ = fmt.Fprintln(stderr, "Flags may also precede RUN_ID. Default mode: --all. Preview is always shown.")
		_, _ = fmt.Fprintln(stderr, "Without --yes, a TTY and y/yes confirmation are required (y/N, default N).")
		_, _ = fmt.Fprintln(stderr, "Code restore first saves rewind-safety/RUN_ID/TIMESTAMP; its id can be used with --code-only to undo the code restore.")
		fs.PrintDefaults()
	}
	var leadingID string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		leadingID, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ids := fs.Args()
	if leadingID != "" {
		ids = append([]string{leadingID}, ids...)
	}
	if len(ids) != 1 {
		_, _ = fmt.Fprintln(stderr, "exactly one run id required; put all flags together before or after it")
		return 2
	}
	runID := ids[0]
	if *targetID == "" {
		_, _ = fmt.Fprintln(stderr, "--to snapshot id is required")
		return 2
	}
	if (*codeOnly && *chatOnly) || (*all && (*codeOnly || *chatOnly)) {
		_, _ = fmt.Fprintln(stderr, "choose only one of --all, --code-only, or --chat-only")
		return 2
	}
	mode := "all"
	if *codeOnly {
		mode = "code-only"
	} else if *chatOnly {
		mode = "chat-only"
	}
	withCode := mode != "chat-only"
	withChat := mode != "code-only"

	cfg, err := load(c)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	run, err := db.GetRun(ctx, runID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "run %q not found: %v\n", runID, err)
		return 1
	}
	workspaceID, err := workspacesnapshot.WorkspaceIDForPath(c.workspaceOrDot())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if run.WorkspaceID == "" {
		_, _ = fmt.Fprintf(stderr, "run %q has no workspace binding; it cannot be rewound from this workspace\n", runID)
		return 1
	}
	if run.WorkspaceID != workspaceID {
		_, _ = fmt.Fprintf(stderr, "run %q belongs to workspace %q, not current workspace %q\n", runID, run.WorkspaceID, workspaceID)
		return 1
	}
	target, err := db.GetWorkspaceSnapshot(ctx, *targetID)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "snapshot %q not found: %v\n", *targetID, err)
		return 1
	}
	if target.WorkspaceID != run.WorkspaceID {
		_, _ = fmt.Fprintf(stderr, "snapshot %q belongs to workspace %q, but run %q belongs to workspace %q\n", target.ID, target.WorkspaceID, runID, run.WorkspaceID)
		return 1
	}
	if withChat && target.EventSeq == nil {
		_, _ = fmt.Fprintf(stderr, "snapshot %q has no event_seq; cannot locate the chat rewind point\n", target.ID)
		return 1
	}
	var snapper *workspacesnapshot.Snapshotter
	if withCode {
		snapper, err = workspacesnapshot.Open(c.workspaceOrDot(), db)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}

	var preview workspacesnapshot.DiffSummary
	if withCode {
		preview, err = snapper.DiffCurrent(ctx, target)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "preview workspace changes: %v\n", err)
			return 1
		}
		_, _ = fmt.Fprintln(stdout, "Code changes (current → target):")
		if len(preview.Changes) == 0 {
			_, _ = fmt.Fprintln(stdout, "  (none)")
		}
		for _, change := range preview.Changes {
			_, _ = fmt.Fprintf(stdout, "  %s %s\n", change.Status, change.Path)
		}
	}
	var deletedEvents int
	var firstSeq, lastSeq int64
	var previewEvents []storage.Event
	if withChat {
		_, checkpointSeq, err := db.LatestCheckpointAtOrBefore(ctx, runID, *target.EventSeq)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "cannot restore checkpoint at snapshot %q (event seq %d): %v\n", target.ID, *target.EventSeq, err)
			return 1
		}
		events, err := db.ListEvents(ctx, runID, 0)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "preview chat events: %v\n", err)
			return 1
		}
		previewEvents = events
		for _, event := range events {
			if event.Seq > *target.EventSeq {
				deletedEvents++
				if firstSeq == 0 || event.Seq < firstSeq {
					firstSeq = event.Seq
				}
				if event.Seq > lastSeq {
					lastSeq = event.Seq
				}
			}
		}
		_, _ = fmt.Fprintf(stdout, "Chat events to delete: %d", deletedEvents)
		if deletedEvents > 0 {
			_, _ = fmt.Fprintf(stdout, " (seq %d–%d)", firstSeq, lastSeq)
		}
		_, _ = fmt.Fprintln(stdout)
		_, _ = fmt.Fprintf(stdout, "Checkpoint to restore: seq %d (latest saved at or before target seq %d)\n", checkpointSeq, *target.EventSeq)
	}
	if !*yes {
		if !readerIsTTY(stdin) {
			_, _ = fmt.Fprintln(stderr, "rewind requires confirmation on a TTY; re-run with --yes to proceed")
			return 1
		}
		_, _ = fmt.Fprint(stdout, "Apply rewind? [y/N]: ")
		line, _, readErr := readChatLine(bufio.NewReader(stdin))
		if readErr != nil {
			_, _ = fmt.Fprintf(stderr, "read confirmation: %v\n", readErr)
			return 1
		}
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			_, _ = fmt.Fprintln(stdout, "Rewind cancelled.")
			return 0
		}
	}

	owner := domain.NewID("rewind_")
	now := time.Now().UTC()
	if _, err := db.AcquireLease(ctx, runID, owner, now, now.Add(10*time.Minute)); err != nil {
		_, _ = fmt.Fprintf(stderr, "cannot rewind while run is in use: %v\n", err)
		return 1
	}
	defer func() { _ = db.ReleaseLease(ctx, runID, owner) }()
	// The event log can advance while the user considers the prompt.
	if withChat {
		current, err := db.ListEvents(ctx, runID, 0)
		if err != nil || !sameRewindEvents(current, previewEvents) {
			_, _ = fmt.Fprintf(stderr, "chat changed since preview or cannot be read; re-run rewind: %v\n", err)
			return 1
		}
	}

	safetyID := "none"
	var safetySnapshot storage.WorkspaceSnapshot
	if withCode {
		store := rewindSnapshotStore{RecordStore: db, events: db, runID: runID}
		safeSnapper, err := workspacesnapshot.Open(c.workspaceOrDot(), store)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "open safety snapshotter: %v\n", err)
			return 1
		}
		safetySnapshot, err = safeSnapper.Snapshot(ctx, "rewind-safety/"+runID+"/"+time.Now().UTC().Format("20060102T150405.000000000Z"))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "create rewind safety snapshot: %v\n", err)
			return 1
		}
		safetyID = safetySnapshot.ID
		_, _ = fmt.Fprintf(stdout, "Safety snapshot: %s\n", safetyID)
	}

	if withCode {
		if err := snapper.Restore(ctx, target); err != nil {
			_, _ = fmt.Fprintf(stderr, "restore workspace: %v\n", err)
			return 1
		}
	}
	if withChat {
		result, err := db.RewindChat(ctx, runID, *target.EventSeq)
		if err != nil {
			if withCode {
				if restoreErr := snapper.Restore(ctx, safetySnapshot); restoreErr != nil {
					_, _ = fmt.Fprintf(stderr, "rewind chat: %v; restore workspace safety point %s: %v\n", err, safetyID, restoreErr)
					return 1
				}
			}
			_, _ = fmt.Fprintf(stderr, "rewind chat: %v\n", err)
			return 1
		}
		deletedEvents = result.DeletedEvents
	}
	_, _ = fmt.Fprintf(stdout, "Rewind complete: mode=%s target_snapshot=%s code_files=%d deleted_events=%d safety_snapshot=%s\n", mode, target.ID, len(preview.Changes), deletedEvents, safetyID)
	return 0
}

func (c *commonFlags) workspaceOrDot() string {
	if c.workspace == "" {
		return "."
	}
	return c.workspace
}

func readerIsTTY(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd())
}

func sameRewindEvents(a, b []storage.Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Seq != b[i].Seq {
			return false
		}
	}
	return true
}
