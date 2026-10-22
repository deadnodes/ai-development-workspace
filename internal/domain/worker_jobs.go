package domain

import "time"

const WorkerOperationKind = "WORKER_EXECUTION"

// WorkerExecution is operator-owned, immutable recipe content. Commands are
// literal argv, never request parameters or shell templates.
type WorkerExecution struct {
	RuntimeRootFS  *WorkerRuntimeRootFS `json:"runtime_rootfs,omitempty"`
	Kind           string               `json:"kind"`
	ToolchainImage string               `json:"toolchain_image"`
	BuildKitImage  string               `json:"buildkit_image,omitempty"`
	Platform       string               `json:"platform"`
	CheckArgv      []string             `json:"check_argv"`
	BuildArgv      []string             `json:"build_argv,omitempty"`
	OutputFile     string               `json:"output_file,omitempty"`
	ImagePath      string               `json:"image_path,omitempty"`
	Entrypoint     []string             `json:"entrypoint,omitempty"`
	Network        string               `json:"network"`
	TimeoutSeconds int                  `json:"timeout_seconds"`
	MemoryMB       int                  `json:"memory_mb"`
	StorageMB      int                  `json:"storage_mb"`
	CPUs           int                  `json:"cpus"`
	PIDs           int                  `json:"pids"`
	SourceBytes    int64                `json:"source_bytes"`
	OutputBytes    int64                `json:"output_bytes"`
	LogBytes       int64                `json:"log_bytes"`
}

// WorkerRuntimeRootFS pins an operator-prepared bundle baked into the immutable
// toolchain. Local paths and bundle bytes are never control-plane inputs.
type WorkerRuntimeRootFS struct {
	Hash             string `json:"hash"`
	ArchiveBytes     int64  `json:"archive_bytes"`
	MaxExpandedBytes int64  `json:"max_expanded_bytes"`
	MaxEntries       int    `json:"max_entries"`
	InputsHash       string `json:"inputs_hash"`
}

// WorkerLocalOCI is an executor assertion about a verified local archive. It
// does not assert registry presence, publication or deployment.
type WorkerLocalOCI struct {
	RuntimeRootFS  *WorkerRuntimeRootFS `json:"runtime_rootfs,omitempty"`
	JobID          string               `json:"job_id"`
	AttemptID      string               `json:"attempt_id"`
	InputHash      string               `json:"input_hash"`
	SourceSHA      string               `json:"source_sha"`
	RecipeHash     string               `json:"recipe_hash"`
	ConfigRevision string               `json:"config_revision"`
	ToolchainImage string               `json:"toolchain_image"`
	Platform       string               `json:"platform"`
	ArchiveHash    string               `json:"archive_hash"`
	LayoutHash     string               `json:"layout_hash"`
	IndexDigest    string               `json:"index_digest"`
	ManifestDigest string               `json:"manifest_digest"`
	ArchiveBytes   int64                `json:"archive_bytes"`
}

type WorkerSourceProof struct {
	ParentHash   string `json:"parent_hash,omitempty"`
	DeliveryID   string `json:"delivery_id"`
	ParentID     string `json:"parent_id,omitempty"`
	SnapshotHash string `json:"snapshot_hash"`
}
type WorkerInputs struct {
	Registry       *WorkerRegistryInputs `json:"registry,omitempty"`
	Check          *WorkerCheckBinding   `json:"check,omitempty"`
	SourceProof    *WorkerSourceProof    `json:"source_proof,omitempty"`
	RecipeID       string                `json:"recipe_id"`
	RecipeRevision string                `json:"recipe_revision"`
	RecipeHash     string                `json:"recipe_hash"`
	ConfigRevision string                `json:"config_revision"`
	RepositoryID   string                `json:"repository_id"`
	RepositoryURL  string                `json:"repository_url"`
	RevisionID     string                `json:"revision_id"`
	SourceSHA      string                `json:"source_sha"`
	Trust          string                `json:"trust"`
	Parameters     map[string]any        `json:"parameters"`
	Execution      *WorkerExecution      `json:"execution"`
	Publication    *WorkerPublication    `json:"publication,omitempty"`
}
type WorkerJob struct {
	CheckOperationID string `json:"check_operation_id,omitempty"`
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

type WorkerCheckBinding struct {
	Name            string `json:"name"`
	ExpectedAppID   int64  `json:"expected_app_id"`
	FinalizeSeconds int    `json:"finalize_seconds"`
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
	RecipeID     string `json:"recipe_id,omitempty"`
	RecipeHash   string `json:"recipe_hash,omitempty"`
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
	Registry *WorkerRegistryReport `json:"registry,omitempty"`
	WorkerAttemptRequest
	Result       string                   `json:"result"`
	EvidenceHash string                   `json:"evidence_hash,omitempty"`
	LocalOCI     *WorkerLocalOCI          `json:"local_oci,omitempty"`
	Publication  *WorkerPublicationReport `json:"publication,omitempty"`
}

// WorkerPublication freezes authenticated build evidence; never a local path or credential.
type WorkerPublication struct {
	ComponentID string         `json:"component_id"`
	Profile     string         `json:"profile"`
	Origin      string         `json:"origin"`
	Repository  string         `json:"repository"`
	Receipt     WorkerLocalOCI `json:"receipt"`
}
type WorkerPublicationReport struct {
	ManifestDigest string `json:"manifest_digest"`
}
type NativeArtifact struct {
	PublicationJobID     string                `json:"publication_job_id"`
	PublicationAttemptID string                `json:"publication_attempt_id"`
	PublicationInputHash string                `json:"publication_input_hash"`
	Profile              string                `json:"profile"`
	Receipt              WorkerLocalOCI        `json:"receipt"`
	Observation          NativeObservation     `json:"observation"`
	Reconciliation       *NativeReconciliation `json:"reconciliation,omitempty"`
}
type NativeObservation struct {
	ManifestDigest string    `json:"manifest_digest"`
	ConfigDigest   string    `json:"config_digest"`
	LayerDigest    string    `json:"layer_digest"`
	Platform       string    `json:"platform"`
	Bytes          int64     `json:"bytes"`
	ObservedAt     time.Time `json:"observed_at"`
}
type NativeReconciliation struct {
	Actor         string    `json:"actor"`
	At            time.Time `json:"at"`
	AttemptStatus string    `json:"attempt_status"`
}
type NativeQuarantine struct {
	Meta
	RestoreID string `json:"restore_id"`
}
