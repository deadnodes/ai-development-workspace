package delivery

import (
	"context"
	"time"
)

// ActionsRun is workflow metadata, not proof that an image was published. HeadSHA
// identifies the workflow checkout/ref; a workflow may build different source.
// Artifact provenance still requires the configured result report and registry.
type ActionsRun struct {
	ID         int64     `json:"id"`
	WorkflowID int64     `json:"workflow_id"`
	Name       string    `json:"name"`
	HeadSHA    string    `json:"head_sha"`
	HeadBranch string    `json:"head_branch"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
}

// ActionsInventoryProvider optionally exposes a bounded read-only inventory of
// recent successful runs, including builds initiated outside the Control Plane.
type ActionsInventoryProvider interface {
	ListRecentBuilds(context.Context, Connection, string) ([]ActionsRun, error)
}
