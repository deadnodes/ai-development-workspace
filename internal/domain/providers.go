package domain

// TARGET PORTS ONLY: no implementation or runtime call sites exist yet. Product
// grants, approvals and CAS checks must be enforced before adapter mutation.
import (
	"context"
	"time"
)

type ProviderScope struct{ ProductID, ConnectionID string }
type PinnedHead struct{ RepositoryID, Ref, ExpectedCommit string }

// ExpectedHeads pin every relevant source/target; creation pins its base and
// additionally requires target absence. OperationID is the idempotency key.
type MutationContext struct {
	Scope                   ProviderScope
	OperationID, Actor      string
	ExpectedHeads           []PinnedHead
	ExpectedResourceVersion string
}
type GitReferenceRequest struct {
	Scope             ProviderScope
	RepositoryID, Ref string
}
type GitCompareRequest struct {
	Scope                                ProviderScope
	RepositoryID, BaseCommit, HeadCommit string
}
type GitComparison struct {
	BaseCommit, HeadCommit, MergeBaseCommit string
	Ahead, Behind                           int
	AncestryKnown, FastForwardPossible      bool
	ConflictingPaths                        []string
}
type GitMutationRequest struct {
	Mutation                                         MutationContext
	RepositoryID, SourceRef, SourceCommit, TargetRef string
	RequireTargetAbsent                              bool
}
type GitOperationState string

const (
	GitOperationPending     GitOperationState = "pending"
	GitOperationSucceeded   GitOperationState = "succeeded"
	GitOperationConflict    GitOperationState = "conflict"
	GitOperationDiverged    GitOperationState = "diverged"
	GitOperationHeadChanged GitOperationState = "expected_head_changed"
	GitOperationFailed      GitOperationState = "failed"
)

type GitOperationResult struct {
	OperationID                                   string
	Kind                                          GitOperationKind
	State                                         GitOperationState
	PreviousHead, ResultHead, ProviderOperationID string
	ConflictPaths                                 []string
}
type ProviderOperationRequest struct {
	Scope                            ProviderScope
	OperationID, ProviderOperationID string
}
type PullRequestQuery struct {
	Scope            ProviderScope
	RepositoryID, ID string
}
type PullRequestMutation struct {
	Mutation                                        MutationContext
	RepositoryID, SourceRef, TargetRef, Title, Body string
}
type ProviderCheck struct {
	ID, Name, Commit, Status, EvidenceURL string
	ObservedAt                            time.Time
}
type GitProvider interface {
	Repositories(context.Context, ProviderScope) ([]Repository, error)
	Branch(context.Context, GitReferenceRequest) (BranchState, error)
	Compare(context.Context, GitCompareRequest) (GitComparison, error)
	Commit(context.Context, GitReferenceRequest) (Commit, error)
	PullRequest(context.Context, PullRequestQuery) (PullRequest, error)
	Checks(context.Context, GitReferenceRequest) ([]ProviderCheck, error)
	CreateBranch(context.Context, GitMutationRequest) (GitOperationResult, error)
	DeleteBranch(context.Context, GitMutationRequest) (GitOperationResult, error)
	FastForward(context.Context, GitMutationRequest) (GitOperationResult, error)
	Merge(context.Context, GitMutationRequest) (GitOperationResult, error)
	Rebase(context.Context, GitMutationRequest) (GitOperationResult, error)
	ForceUpdate(context.Context, GitMutationRequest) (GitOperationResult, error)
	OperationStatus(context.Context, ProviderOperationRequest) (GitOperationResult, error)
	CreatePullRequest(context.Context, PullRequestMutation) (PullRequest, error)
}

type BuildRequest struct {
	Mutation                                                                                  MutationContext
	ProductComponentID, DefinitionID, DefinitionCommit, Workflow, SourceCommit, CompositionID string
	Parameters                                                                                map[string]string
	Secrets                                                                                   []SecretReference
}
type BuildQuery struct {
	Scope         ProviderScope
	ProviderRunID string
}
type BuildCancellation struct {
	Mutation      MutationContext
	ProviderRunID string
}
type BuildLogReference struct {
	Name, URI, Digest string
	ObservedAt        time.Time
}
type CIProvider interface {
	Artifacts(context.Context, BuildQuery) ([]ArtifactRecord, error)
	Logs(context.Context, BuildQuery) ([]BuildLogReference, error)
	Start(context.Context, BuildRequest) (BuildRun, error)
	Run(context.Context, BuildQuery) (BuildRun, error)
	Cancel(context.Context, BuildCancellation) (BuildRun, error)
}

type ArtifactQuery struct {
	Scope               ProviderScope
	URI, ExpectedDigest string
}
type ArtifactInspection struct {
	RequestedDigest, ObservedDigest string
	Observation                     ArtifactObservation
}
type ArtifactProvider interface {
	Inspect(context.Context, ArtifactQuery) (ArtifactInspection, error)
}

type GitOpsImageUpdate struct{ ComponentID, ArtifactID, ExpectedDigest string }

// Planning is read-only and captures the exact diff and base to approve.
type GitOpsPlanRequest struct {
	Scope                       ProviderScope
	Mapping                     GitOpsMapping
	Images                      []GitOpsImageUpdate
	DeploymentRunID, BaseCommit string
}
type GitOpsChangePlan struct {
	ID                                            string
	Scope                                         ProviderScope
	Mapping                                       GitOpsMapping
	Images                                        []GitOpsImageUpdate
	DeploymentRunID, BaseCommit, Diff, DiffDigest string
	CreatedAt                                     time.Time
}
type ApprovedGitOpsPlan struct {
	Plan                                           GitOpsChangePlan
	ApprovedPlanID, ApprovedDiffDigest, ApprovedBy string
	ApprovedAt                                     time.Time
}

// Apply must reject altered plan/diff and stale expected branch heads.
type GitOpsApplyRequest struct {
	Mutation     MutationContext
	ApprovedPlan ApprovedGitOpsPlan
}
type GitOpsChangeResult struct {
	OperationID, RepositoryID, PreviousCommit, Commit, PullRequestID string
	State                                                            GitOperationState
}
type GitOpsReadRequest struct {
	Scope   ProviderScope
	Mapping GitOpsMapping
	Commit  string
}
type GitOpsDesiredState struct {
	RepositoryID, Commit string
	Images               []GitOpsImageUpdate
	ObservedAt           time.Time
}
type GitOpsProvider interface {
	Read(context.Context, GitOpsReadRequest) (GitOpsDesiredState, error)
	PlanChange(context.Context, GitOpsPlanRequest) (GitOpsChangePlan, error)
	ApplyChange(context.Context, GitOpsApplyRequest) (GitOpsChangeResult, error)
}

type FluxQuery struct {
	Scope                                                   ProviderScope
	Namespace, ResourceKind, ResourceName, ExpectedRevision string
}
type FluxObservation struct {
	Namespace, ResourceKind, ResourceName, ObservedRevision string
	Known, Ready                                            bool
	Message                                                 string
	ObservedAt                                              time.Time
}
type FluxProvider interface {
	ReconciliationStatus(context.Context, FluxQuery) (FluxObservation, error)
}
type RuntimeQuery struct {
	Scope              ProviderScope
	Cluster, Namespace string
	ComponentIDs       []string
	ExpectedDigests    map[string]string
}
type RuntimeComponentObservation struct {
	ComponentID, Workload, ImageDigest string
	Known, Healthy                     bool
	ReadyReplicas, DesiredReplicas     int
	Message                            string
	ObservedAt                         time.Time
}
type KubernetesProvider interface {
	RuntimeStatus(context.Context, RuntimeQuery) ([]RuntimeComponentObservation, error)
}
