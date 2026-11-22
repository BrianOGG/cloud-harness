package agent

import (
	"context"

	"github.com/cloud-harness/cloud-harness/internal/security"
)

// EventConfigUntrusted records that a workspace control file was present at
// run start but ignored because the user has not trusted its content.
const EventConfigUntrusted = "config.untrusted"

// UntrustedNotice is the event payload. It never contains file content.
type UntrustedNotice struct {
	File    string              `json:"file"`
	Path    string              `json:"path"`
	State   security.TrustState `json:"state"`
	SHA256  string              `json:"sha256"`
	Reason  string              `json:"reason,omitempty"`
	Message string              `json:"message"`
}

// UntrustedMessage states what is being ignored and how to change that.
func UntrustedMessage(status security.TrustStatus) string {
	effect := "its hooks are not run"
	if status.File == security.TrustPolicyFile {
		effect = "its allow and ask rules are not applied (deny rules still are)"
	}
	why := "is not trusted"
	if status.State == security.TrustChanged {
		why = "changed since it was trusted"
	}
	msg := "workspace " + status.File + " " + why + ": " + effect + ". Review " + status.Path + ", then run `cloud-harness trust approve`."
	if status.Reason != "" {
		msg += " (" + status.Reason + ")"
	}
	return msg
}

func (m *Manager) noteUntrusted(ctx context.Context, runID string, statuses ...*security.TrustStatus) error {
	for _, status := range statuses {
		if status == nil || !status.Pending() {
			continue
		}
		notice := UntrustedNotice{File: status.File, Path: status.Path, State: status.State, SHA256: status.SHA256, Reason: status.Reason, Message: UntrustedMessage(*status)}
		if err := m.event(ctx, runID, EventConfigUntrusted, notice); err != nil {
			return err
		}
	}
	return nil
}
