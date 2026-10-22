package delivery

import (
	"context"
	"time"
)

// CheckRunProvider is an optional, exact-identity GitHub App publication port.
// The application owns durable write intent and recovery; adapters never retry
// a POST/PATCH or substitute another check after an uncertain response.
type CheckRunProvider interface {
	FindCheckRun(context.Context, Connection, CheckRunIdentity) (CheckRunObservation, error)
	ReadCheckRun(context.Context, Connection, CheckRunIdentity, int64) (CheckRunObservation, error)
	CreateCheckRun(context.Context, Connection, CheckRunWrite) (CheckRunObservation, error)
	UpdateCheckRun(context.Context, Connection, CheckRunWrite) (CheckRunObservation, error)
}
type CheckRunIdentity struct {
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	HeadSHA      string `json:"head_sha"`
	Name         string `json:"name"`
	ExternalID   string `json:"external_id"`
	AppID        int64  `json:"app_id"`
}
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}
type CheckRunWrite struct {
	Identity    CheckRunIdentity `json:"identity"`
	ID          int64            `json:"id"`
	Status      string           `json:"status"`
	Conclusion  string           `json:"conclusion"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
	Output      CheckRunOutput   `json:"output"`
}
type CheckRunObservation struct {
	Found       bool             `json:"found"`
	ID          int64            `json:"id"`
	Identity    CheckRunIdentity `json:"identity"`
	Status      string           `json:"status"`
	Conclusion  string           `json:"conclusion"`
	StartedAt   time.Time        `json:"started_at"`
	CompletedAt time.Time        `json:"completed_at"`
	Output      CheckRunOutput   `json:"output"`
}
