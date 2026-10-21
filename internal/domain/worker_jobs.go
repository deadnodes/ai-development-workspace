package domain

import "time"

const WorkerOperationKind = "WORKER_EXECUTION"

type WorkerInputs struct {
	RecipeID       string         `json:"recipe_id"`
	RecipeRevision string         `json:"recipe_revision"`
	RecipeHash     string         `json:"recipe_hash"`
	ConfigRevision string         `json:"config_revision"`
	RepositoryID   string         `json:"repository_id"`
	RepositoryURL  string         `json:"repository_url"`
	RevisionID     string         `json:"revision_id"`
	SourceSHA      string         `json:"source_sha"`
	Trust          string         `json:"trust"`
	Parameters     map[string]any `json:"parameters"`
}
type WorkerJob struct {
	Meta
	OperationID           string       `json:"operation_id"`
	RequestKey            string       `json:"request_key"`
	RequestHash           string       `json:"request_hash"`
	IntegrationID         string       `json:"integration_id"`
	Capability            string       `json:"capability"`
	Inputs                WorkerInputs `json:"inputs"`
	InputHash             string       `json:"input_hash"`
	Status                string       `json:"status"`
	Deadline              time.Time    `json:"deadline"`
	MaxAttempts           int          `json:"max_attempts"`
	Fence                 uint64       `json:"fence"`
	CancellationRequested bool         `json:"cancellation_requested"`
}
type WorkerAttempt struct {
	Meta
	JobID      string        `json:"job_id"`
	Principal  string        `json:"principal"`
	Fence      uint64        `json:"fence"`
	InputHash  string        `json:"input_hash"`
	LeaseUntil time.Time     `json:"lease_until"`
	Status     string        `json:"status"`
	Report     *WorkerReport `json:"report,omitempty"`
}
type WorkerLease struct {
	Job     WorkerJob     `json:"job"`
	Attempt WorkerAttempt `json:"attempt"`
}
type WorkerClaim struct {
	ProductID    string `json:"product_id"`
	RepositoryID string `json:"repository_id"`
	Capability   string `json:"capability"`
}
type WorkerAttemptRequest struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	Fence     uint64 `json:"fence"`
	InputHash string `json:"input_hash"`
}
type WorkerReport struct {
	WorkerAttemptRequest
	Result       string `json:"result"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
}
