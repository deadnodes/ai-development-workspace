// Package delivery contains the concrete MVP external-provider contract.
package delivery

import (
	"context"
	"time"
)

type Connection struct {
	ID                    string `json:"id"`
	AppID                 int64  `json:"app_id"`
	InstallationID        int64  `json:"installation_id"`
	PrivateKeyRef         string `json:"private_key_ref"`
	Owner                 string `json:"owner"`
	APIURL                string `json:"api_url"`
	RegistryCredentialRef string `json:"registry_credential_ref,omitempty"`
}
type RepositoryInfo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	URL           string `json:"url"`
	Private       bool   `json:"private"`
}
type BranchInfo struct {
	Name      string `json:"name"`
	SHA       string `json:"sha"`
	Protected bool   `json:"protected"`
}
type CommitInfo struct {
	SHA        string    `json:"sha"`
	Message    string    `json:"message"`
	Author     string    `json:"author"`
	AuthoredAt time.Time `json:"authored_at"`
	URL        string    `json:"url"`
}
type CompareResult struct {
	Commits []CommitInfo `json:"commits"`
	BaseSHA string       `json:"base_sha"`
	HeadSHA string       `json:"head_sha"`
	Ahead   int          `json:"ahead"`
	Behind  int          `json:"behind"`
	Status  string       `json:"status"`
}
type BuildRequest struct {
	WorkflowSHA string            `json:"workflow_sha,omitempty"`
	Repository  string            `json:"repository"`
	Workflow    string            `json:"workflow"`
	Ref         string            `json:"ref"`
	SourceSHA   string            `json:"source_sha"`
	ImageTag    string            `json:"image_tag"`
	OperationID string            `json:"operation_id"`
	Inputs      map[string]string `json:"inputs"`
	RequestedAt time.Time         `json:"requested_at"`
}
type DispatchResult struct {
	Correlation   string `json:"correlation"`
	ProviderRunID int64  `json:"provider_run_id"`
}
type RunResult struct {
	Found      bool              `json:"found"`
	ID         int64             `json:"id"`
	Status     string            `json:"status"`
	Conclusion string            `json:"conclusion"`
	HeadSHA    string            `json:"head_sha"`
	URL        string            `json:"url"`
	Artifacts  map[string]string `json:"artifacts"`
}
type ArtifactRequest struct {
	EvidenceRepository  string `json:"evidence_repository,omitempty"`
	EvidenceRunID       int64  `json:"evidence_run_id,omitempty"`
	EvidenceOperationID string `json:"evidence_operation_id,omitempty"`
	SourceSHA           string `json:"source_sha,omitempty"`
	Repository          string `json:"repository"`
	Tag                 string `json:"tag"`
	ExpectedDigest      string `json:"expected_digest"`
	CredentialRef       string `json:"credential_ref,omitempty"`
}
type ArtifactResult struct {
	Available  bool      `json:"available"`
	Digest     string    `json:"digest"`
	URI        string    `json:"uri"`
	ObservedAt time.Time `json:"observed_at"`
}
type GitOpsRequest struct {
	Repository      string `json:"repository"`
	Ref             string `json:"ref"`
	Path            string `json:"path"`
	ImageRepository string `json:"image_repository"`
	ImageField      string `json:"image_field"`
	DigestField     string `json:"digest_field"`
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
}
type GitOpsSnapshot struct {
	ImageRepository string `json:"image_repository"`
	HeadSHA         string `json:"head_sha"`
	BlobSHA         string `json:"blob_sha"`
	Content         string `json:"content"`
	Digest          string `json:"digest"`
}
type GitOpsApply struct {
	Request         GitOpsRequest `json:"request"`
	ExpectedHeadSHA string        `json:"expected_head_sha"`
	ExpectedBlobSHA string        `json:"expected_blob_sha"`
	Digest          string        `json:"digest"`
	OperationID     string        `json:"operation_id"`
	Message         string        `json:"message"`
}
type GitOpsResult struct {
	CommitSHA      string `json:"commit_sha"`
	BlobSHA        string `json:"blob_sha"`
	URL            string `json:"url"`
	AlreadyApplied bool   `json:"already_applied"`
}
type Provider interface {
	TestConnection(context.Context, Connection) error
	DiscoverRepositories(context.Context, Connection) ([]RepositoryInfo, error)
	Branches(context.Context, Connection, string) ([]BranchInfo, error)
	Compare(context.Context, Connection, string, string, string) (CompareResult, error)
	Dispatch(context.Context, Connection, BuildRequest) (DispatchResult, error)
	FindRun(context.Context, Connection, BuildRequest) (RunResult, error)
	InspectArtifact(context.Context, Connection, ArtifactRequest) (ArtifactResult, error)
	ReadGitOps(context.Context, Connection, GitOpsRequest) (GitOpsSnapshot, error)
	ApplyGitOps(context.Context, Connection, GitOpsApply) (GitOpsResult, error)
}
