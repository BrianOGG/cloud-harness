// Package storage is the persistence boundary. SQLite transactions stay inside
// the driver implementation; callers never see a database handle.
package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// Session is a long-lived conversation.
type Session struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SessionSummary is a session and its run aggregates for read-only listings.
type SessionSummary struct {
	ID              string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	RunCount        int
	LatestRunStatus domain.RunStatus
	LatestRunAt     *time.Time
}

// Turn is one user input.
type Turn struct {
	ID        string
	SessionID string
	UserInput string
	Status    string
	CreatedAt time.Time
}

// Run is the persisted execution record.
type Run struct {
	ID              string
	ParentRunID     string
	SessionID       string
	TurnID          string
	WorkspaceID     string
	WorktreeBaseSHA string
	WorktreeBranch  string
	WorktreePath    string
	WorktreeState   string // Empty until a successful merge or explicit discard.
	Status          domain.RunStatus
	WaitReason      domain.WaitReason
	Budget          domain.Budget
	Version         int64
	FinalAnswer     string
	Error           *domain.Error
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ChatRewindResult describes the persistent events removed by a rewind.
type ChatRewindResult struct {
	DeletedEvents int
	FirstSeq      int64
	LastSeq       int64
	CheckpointSeq int64
}

// Lease is the single-owner scheduler lock for a run.
type Lease struct {
	RunID        string
	OwnerID      string
	LeaseVersion int64
	ExpiresAt    time.Time
	HeartbeatAt  time.Time
}

// Event is an append-only persistent fact. Ephemeral stream deltas are not events.
type Event struct {
	ID           string
	RunID        string
	Seq          int64
	BatchID      string
	InvocationID string
	Type         string
	Payload      json.RawMessage
	CreatedAt    time.Time
	Persistent   bool
}

// StoredResponse is a model response with opaque bytes split out.
type StoredResponse struct {
	ID                 string
	RunID              string
	StepID             string
	Provider           string
	ProviderResponseID string
	Complete           bool
	Usage              domain.Usage
	Items              []domain.VisibleItem
	Opaque             []byte
	Action             domain.Action
	CreatedAt          time.Time
}

// StoredBatch is the persisted tool batch.
type StoredBatch struct {
	ID              string
	ModelResponseID string
	RunID           string
	Status          domain.BatchStatus
	Action          domain.Action
	NextOrdinal     int
	ProtocolError   *domain.Error
	Invocations     []domain.Invocation
}

// HistoryDoc is one searchable record. RawText is authoritative; the FTS
// column is rebuildable from it.
type HistoryDoc struct {
	SourceID  string
	SessionID string
	RawText   string
	Metadata  json.RawMessage
	CreatedAt time.Time
}

// Hit is a search result. MatchKind is "exact" or "fts".
type Hit struct {
	SourceID  string
	SessionID string
	RawText   string
	MatchKind string
}

// SummaryJob is a recoverable post-hoc summary. The run answer is already stored.
type SummaryJob struct {
	ID        string
	SessionID string
	TurnID    string
	RunID     string
	Status    string
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TurnSummary is a persisted turn summary with its turn timestamp.
type TurnSummary struct {
	TurnID     string
	Summary    string
	SourceRefs string
	CreatedAt  time.Time
}

// WorkspaceSnapshot records one Git tree captured for a workspace.
type WorkspaceSnapshot struct {
	ID            string
	RunID         string
	WorkspaceID   string
	WorkspacePath string
	TreeOID       string
	Label         string
	EventSeq      *int64
	FileCount     int64
	TotalBytes    int64
	CreatedAt     time.Time
}

// HistoryCatalogEntry is a lightweight summary of one session history record.
type HistoryCatalogEntry struct {
	SourceID  string    `json:"source_id"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	Summary   string    `json:"summary"`
}

// PruneRunsResult details the rows removed by a run pruning operation.
type PruneRunsResult struct {
	RunsDeleted        int                 `json:"runs_deleted"`
	RunsToDelete       int                 `json:"runs_to_delete"`
	SnapshotsDeleted   int                 `json:"snapshots_deleted"`
	SnapshotsToDelete  int                 `json:"snapshots_to_delete"`
	UnassignedRetained int                 `json:"unassigned_retained"`
	Snapshots          []WorkspaceSnapshot `json:"-"`
	Tables             map[string]int      `json:"tables"`
	WALCheckpoint      string              `json:"wal_checkpoint,omitempty"`
}

// Store is the single persisted source of truth used by the run manager.
type Store interface {
	Close() error
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, id string) (Session, error)
	ListSessions(ctx context.Context) ([]SessionSummary, error)
	CreateTurn(ctx context.Context, t Turn) error
	CreateRun(ctx context.Context, r Run) error
	GetRun(ctx context.Context, id string) (Run, error)
	ListRuns(ctx context.Context, sessionID string) ([]Run, error)
	ListRunsByStatus(ctx context.Context, status domain.RunStatus) ([]Run, error)
	CompareAndSwapRun(ctx context.Context, next Run, expectedVersion int64) error
	Charge(ctx context.Context, runID string, kind domain.ChargeKind, n int) error
	ChargePair(ctx context.Context, parentID, childID string, kind domain.ChargeKind, n int) error
	AcquireLease(ctx context.Context, runID, owner string, now, expires time.Time) (Lease, error)
	HeartbeatLease(ctx context.Context, runID, owner string, version int64, now, expires time.Time) error
	RequireLease(ctx context.Context, runID, owner string, version int64, now time.Time) error
	ReleaseLease(ctx context.Context, runID, owner string) error
	AppendEvent(ctx context.Context, ev Event) (int64, error)
	ListEvents(ctx context.Context, runID string, afterSeq int64) ([]Event, error)
	SaveModelResponse(ctx context.Context, rec StoredResponse) error
	ListVisibleResponses(ctx context.Context, runID string) ([]StoredResponse, error)
	LoadContinuation(ctx context.Context, responseID string) ([]byte, error)
	SaveBatch(ctx context.Context, batch StoredBatch) error
	UpdateBatch(ctx context.Context, batch StoredBatch) error
	LoadBatches(ctx context.Context, runID string) ([]StoredBatch, error)
	SaveTurnSummary(ctx context.Context, turnID, summary, sourceRefs string) error
	GetTurnSummary(ctx context.Context, turnID string) (string, error)
	SaveMemory(ctx context.Context, sessionID, stateJSON, sourceRefs string) error
	CommitInvocation(ctx context.Context, runID string, inv domain.Invocation, expectedVersion int64, ev Event) error
	CommitRunTransition(ctx context.Context, next Run, expectedVersion int64, ev Event) error
	SaveCheckpoint(ctx context.Context, runID string, version int64, snapshot []byte, now time.Time) error
	LatestCheckpoint(ctx context.Context, runID string) ([]byte, error)
	LookupIdempotency(ctx context.Context, runID, toolName, key string) (json.RawMessage, bool, error)
	SaveIdempotency(ctx context.Context, runID, toolName, key string, result json.RawMessage, now time.Time) error
	ExtendModelBudget(ctx context.Context, runID string, extra int) error
	SaveApproval(ctx context.Context, a domain.Approval) error
	UpdateApprovalState(ctx context.Context, runID, invocationID, state string) error
	ListApprovals(ctx context.Context, runID string) ([]domain.Approval, error)
	SaveGrant(ctx context.Context, g domain.Grant, now time.Time) error
	ListGrants(ctx context.Context, runID, sessionID string, now time.Time) ([]domain.Grant, error)
	IndexHistory(ctx context.Context, doc HistoryDoc) error
	SearchHistory(ctx context.Context, sessionID, query string, limit int) ([]Hit, error)
	RebuildHistory(ctx context.Context, sessionID string) error
	PruneHistory(ctx context.Context, before time.Time) (int, error)
	PruneRuns(ctx context.Context, before time.Time) (PruneRunsResult, error)
	GetHistory(ctx context.Context, sourceID string) (HistoryDoc, error)
	RecentTurnSummaries(ctx context.Context, sessionID string, limit int) ([]TurnSummary, error)
	ListHistoryCatalog(ctx context.Context, sessionID string, limit int) ([]HistoryCatalogEntry, error)
	LatestMemory(ctx context.Context, sessionID string) (string, int, error)
	CountTurns(ctx context.Context, sessionID string) (int, error)
	SaveSummaryJob(ctx context.Context, job SummaryJob) error
	ListOpenSummaryJobs(ctx context.Context, sessionID string) ([]SummaryJob, error)
	UpdateSummaryJob(ctx context.Context, job SummaryJob) error
}
