package domain

import (
	"releasecontrol/internal/delivery"
	"time"
)

// Concrete persisted MVP configuration; credential fields are references only.
type GitHubConnection struct {
	Connected       bool       `json:"connected"`
	LastTestedAt    *time.Time `json:"last_tested_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	RepositoryCount int        `json:"repository_count"`
	Meta
	Name   string              `json:"name"`
	Config delivery.Connection `json:"config"`
}
type RepositoryBinding struct {
	BaseBranch string `json:"base_branch"`
	Meta
	RepositoryID  string `json:"repository_id"`
	ConnectionID  string `json:"connection_id"`
	FullName      string `json:"full_name"`
	Role          string `json:"role"`
	DefaultBranch string `json:"default_branch"`
}
type NativeBinding struct {
	BuildRecipeID       string         `json:"build_recipe_id"`
	PublicationRecipeID string         `json:"publication_recipe_id"`
	Parameters          map[string]any `json:"parameters"`
}
type NativeDelivery struct {
	ParentHash                string    `json:"parent_hash,omitempty"`
	ConfigRevision            string    `json:"config_revision"`
	BuildRecipeHash           string    `json:"build_recipe_hash"`
	BuildRecipeRevision       string    `json:"build_recipe_revision"`
	PublicationRecipeHash     string    `json:"publication_recipe_hash"`
	PublicationRecipeRevision string    `json:"publication_recipe_revision"`
	Deadline                  time.Time `json:"deadline"`
	BuildJobID                string    `json:"build_job_id,omitempty"`
	PublicationJobID          string    `json:"publication_job_id,omitempty"`
}
type ComponentBuild struct {
	Native         *NativeBinding `json:"native,omitempty"`
	Mode           string         `json:"mode,omitempty"`
	Profile        string         `json:"profile,omitempty"`
	RebuildMissing bool           `json:"rebuild_missing"`
	Meta
	ApplicationID   string            `json:"application_id"`
	ConnectionID    string            `json:"connection_id"`
	Workflow        string            `json:"workflow"`
	ImageRepository string            `json:"image_repository"`
	WorkflowRef     string            `json:"workflow_ref"`
	Inputs          map[string]string `json:"inputs"`
}
type EnvironmentBinding struct {
	GitOpsMode string `json:"gitops_mode"`
	Meta
	EnvironmentID string `json:"environment_id"`
	ApplicationID string `json:"application_id"`
	Purpose       string `json:"purpose"`
	ConnectionID  string `json:"connection_id"`
	RepositoryID  string `json:"repository_id"`
	Ref           string `json:"ref"`
	Path          string `json:"path"`
	ImageField    string `json:"image_field"`
	DigestField   string `json:"digest_field"`
	AllowDeploy   bool   `json:"allow_deploy"`
}
type GitObservation struct {
	Commits []delivery.CommitInfo `json:"commits"`
	Meta
	IntegrationID string    `json:"integration_id"`
	RepositoryID  string    `json:"repository_id"`
	Branch        string    `json:"branch"`
	HeadCommit    string    `json:"head_commit"`
	MainCommit    string    `json:"main_commit"`
	Ahead         int       `json:"ahead"`
	Behind        int       `json:"behind"`
	Status        string    `json:"status"`
	ObservedAt    time.Time `json:"observed_at"`
}
type DeliverySnapshot struct {
	Native           *NativeDelivery        `json:"native,omitempty"`
	Revision         IntegrationRevision    `json:"revision"`
	Application      Application            `json:"application"`
	Build            ComponentBuild         `json:"build"`
	Target           EnvironmentBinding     `json:"target"`
	Environment      Environment            `json:"environment"`
	Source           RepositoryBinding      `json:"source"`
	GitOps           RepositoryBinding      `json:"gitops"`
	SourceConnection delivery.Connection    `json:"source_connection"`
	GitOpsConnection delivery.Connection    `json:"gitops_connection"`
	BuildRequest     delivery.BuildRequest  `json:"build_request"`
	GitOpsRequest    delivery.GitOpsRequest `json:"gitops_request"`
}
type ExternalOperation struct {
	CheckPublication    *CheckPublication           `json:"check_publication,omitempty"`
	RequestKey          string                      `json:"request_key,omitempty"`
	RequestHash         string                      `json:"request_hash,omitempty"`
	Native              *NativeDelivery             `json:"native,omitempty"`
	GitOpsPR            *delivery.GitOpsPullRequest `json:"gitops_pr,omitempty"`
	ExistingArtifact    *DeliveryArtifact           `json:"existing_artifact,omitempty"`
	PreparedDeployments []DeliverySnapshot          `json:"prepared_deployments,omitempty"`
	ParentID            string                      `json:"parent_id,omitempty"`
	ChildIDs            []string                    `json:"child_ids,omitempty"`
	IntegrationIDs      []string                    `json:"integration_ids,omitempty"`
	CompositionSnapshot *Composition                `json:"composition_snapshot,omitempty"`
	Sources             []FlowSource                `json:"sources,omitempty"`
	BuildOnly           bool                        `json:"build_only,omitempty"`
	ReleaseCandidateID  string                      `json:"release_candidate_id,omitempty"`

	DeploymentState string     `json:"deployment_state"`
	RequestedBy     string     `json:"requested_by"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CurrentStep     string     `json:"current_step"`
	Error           string     `json:"error,omitempty"`
	Meta
	Kind           string                   `json:"kind"`
	IntegrationID  string                   `json:"integration_id"`
	RepositoryID   string                   `json:"repository_id,omitempty"`
	EnvironmentID  string                   `json:"environment_id,omitempty"`
	ApplicationID  string                   `json:"application_id,omitempty"`
	Status         string                   `json:"status"`
	Phase          string                   `json:"phase"`
	Detail         string                   `json:"detail"`
	Snapshot       *DeliverySnapshot        `json:"snapshot,omitempty"`
	ExpectedDigest string                   `json:"expected_digest,omitempty"`
	Artifact       *delivery.ArtifactResult `json:"artifact,omitempty"`
	Run            *delivery.RunResult      `json:"run,omitempty"`
	GitOpsBase     *delivery.GitOpsSnapshot `json:"gitops_base,omitempty"`
	GitOpsResult   *delivery.GitOpsResult   `json:"gitops_result,omitempty"`
	Attempts       int                      `json:"attempts"`
	NextAttemptAt  time.Time                `json:"next_attempt_at"`
	LeaseOwner     string                   `json:"lease_owner,omitempty"`
	LeaseUntil     time.Time                `json:"lease_until"`
	FinishedAt     *time.Time               `json:"finished_at,omitempty"`
}

// CheckPublication is a server-frozen destination and bounded provider intent.
// The passive worker operation remains solely an execution projection.
type CheckPublication struct {
	JobID                string                        `json:"job_id"`
	WorkerOperationID    string                        `json:"worker_operation_id"`
	Inputs               WorkerInputs                  `json:"inputs"`
	InputHash            string                        `json:"input_hash"`
	Source               RepositoryBinding             `json:"source"`
	Connection           delivery.Connection           `json:"connection"`
	ProviderRepositoryID int64                         `json:"provider_repository_id,omitempty"`
	Identity             delivery.CheckRunIdentity     `json:"identity"`
	FinalizeDeadline     time.Time                     `json:"finalize_deadline"`
	Observations         int                           `json:"observations"`
	Intent               *delivery.CheckRunWrite       `json:"intent,omitempty"`
	IntentHash           string                        `json:"intent_hash,omitempty"`
	Observed             *delivery.CheckRunObservation `json:"observed,omitempty"`
}
type OperationStep struct {
	Meta
	OperationID string            `json:"operation_id"`
	Phase       string            `json:"phase"`
	Status      string            `json:"status"`
	Detail      string            `json:"detail"`
	Evidence    map[string]string `json:"evidence,omitempty"`
}

// RegisteredRepository is instance-wide identity; Repository remains a stable
// product attachment projection for backwards compatibility.
type RegisteredRepository struct {
	ProviderAPIURL string   `json:"provider_api_url"`
	ConnectionIDs  []string `json:"connection_ids"`
	Meta
	ConnectionID         string    `json:"connection_id"`
	ProviderRepositoryID int64     `json:"provider_repository_id"`
	FullName             string    `json:"full_name"`
	DefaultBranch        string    `json:"default_branch"`
	URL                  string    `json:"url"`
	Private              bool      `json:"private"`
	ObservedAt           time.Time `json:"observed_at"`
}
type ConnectionGrant struct {
	Meta
	ConnectionID string `json:"connection_id"`
}

type DeliveryArtifact struct {
	Provenance string          `json:"provenance,omitempty"`
	Native     *NativeArtifact `json:"native,omitempty"`
	Meta
	OperationID     string                `json:"operation_id"`
	ApplicationID   string                `json:"application_id"`
	RepositoryID    string                `json:"repository_id"`
	SourceCommit    string                `json:"source_commit"`
	ImageRepository string                `json:"image_repository"`
	Tag             string                `json:"tag"`
	Digest          string                `json:"digest"`
	Availability    string                `json:"availability"`
	ObservedAt      time.Time             `json:"observed_at"`
	BuildRequest    delivery.BuildRequest `json:"build_request"`
	BuildRunID      int64                 `json:"build_run_id"`
}
type DeliveryBuildRun struct {
	Meta
	OperationID   string                `json:"operation_id"`
	ApplicationID string                `json:"application_id"`
	Request       delivery.BuildRequest `json:"request"`
	Result        delivery.RunResult    `json:"result"`
}
