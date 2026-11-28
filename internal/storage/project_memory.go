package storage

import "context"

// ProjectMemory holds only text explicitly confirmed by a user. Source IDs
// are immutable even if the underlying conversation is later pruned.
type ProjectMemory struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Content         string `json:"content"`
	SourceSessionID string `json:"source_session_id"`
	SourceRunID     string `json:"source_run_id"`
	SourceAvailable bool   `json:"source_available"`
	UpdatedAt       string `json:"updated_at"`
}

type ProjectMemoryStore interface {
	ListProjectMemories(ctx context.Context, projectID string, limit int) ([]ProjectMemory, error)
	CreateProjectMemory(ctx context.Context, projectID string, source Run, kind, content string) (ProjectMemory, error)
	UpdateProjectMemory(ctx context.Context, projectID, id, kind, content string) error
	DeleteProjectMemory(ctx context.Context, projectID, id string) error
}
