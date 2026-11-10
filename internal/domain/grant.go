package domain

import "time"

// Grant is a scoped authorization. An earlier grant for the same tool does
// not widen a later call; the resource scope has to match this call.
type Grant struct {
	ID            string    `json:"id"`
	Subject       string    `json:"subject"`
	RunID         string    `json:"run_id,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Tool          string    `json:"tool"`
	Op            string    `json:"op"`
	ResourceScope string    `json:"resource_scope,omitempty"`
	ValidUntil    time.Time `json:"valid_until,omitempty"`
	State         string    `json:"state"`
}
