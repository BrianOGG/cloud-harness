package transport

import (
	"net/http"
	"sort"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type approvalView struct {
	RunID          string           `json:"run_id"`
	SessionID      string           `json:"session_id"`
	Status         domain.RunStatus `json:"status"`
	WaitReason     string           `json:"wait_reason"`
	Summary        string           `json:"summary"`
	WaitingSince   time.Time        `json:"waiting_since"`
	WaitingSeconds int64            `json:"waiting_seconds"`
	ApprovalURL    string           `json:"approval_url,omitempty"`
}

func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Manager.Store.ListRunsByStatus(r.Context(), domain.RunWaiting)
	if err != nil {
		writeErr(w, err)
		return
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	queue := make([]approvalView, 0, len(runs))
	for _, run := range runs {
		var reason string
		switch run.WaitReason {
		case domain.WaitApproval:
			reason = "approval"
		case domain.WaitUser:
			reason = "ask_user"
		default:
			// Budget, child, and reconciliation waits are outside the human queue.
			continue
		}
		info, err := s.Manager.WaitInfo(r.Context(), run)
		if err != nil {
			writeErr(w, err)
			return
		}
		events, err := s.Manager.Store.ListEvents(r.Context(), run.ID, 0)
		if err != nil {
			writeErr(w, err)
			return
		}
		since := waitingSince(run, events).UTC()
		seconds := int64(now.Sub(since) / time.Second)
		if seconds < 0 {
			seconds = 0
		}
		queue = append(queue, approvalView{
			RunID: run.ID, SessionID: run.SessionID, Status: run.Status,
			WaitReason: reason, Summary: info.Summary(), WaitingSince: since,
			WaitingSeconds: seconds, ApprovalURL: s.Manager.ApprovalURL(run.ID),
		})
	}
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].WaitingSince.Equal(queue[j].WaitingSince) {
			return queue[i].RunID < queue[j].RunID
		}
		return queue[i].WaitingSince.Before(queue[j].WaitingSince)
	})
	writeJSON(w, http.StatusOK, queue)
}

func waitingSince(run storage.Run, events []storage.Event) time.Time {
	// Events are in sequence order. A resumed run can enter WAITING repeatedly;
	// only the latest transition starts the current wait, even if updated_at
	// changes later. Older records without events fall back to updated_at.
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "run.WAITING" && !events[i].CreatedAt.IsZero() {
			return events[i].CreatedAt
		}
	}
	return run.UpdatedAt
}
