package domain

// TARGET CONTRACTS ONLY: these types are not in State, persisted, transported or
// wired to adapters yet. They describe the incremental lifecycle architecture;
// their presence does not imply an implemented operation or provider capability.
import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// Component preserves the MVP Application identity and storage contract.
type Component = Application

type Instance struct {
	ID, Name      string
	ConnectionIDs []string
}
type ProviderKind string

const (
	ProviderGit        ProviderKind = "git"
	ProviderCI         ProviderKind = "ci"
	ProviderArtifact   ProviderKind = "artifact"
	ProviderGitOps     ProviderKind = "gitops"
	ProviderFlux       ProviderKind = "flux"
	ProviderKubernetes ProviderKind = "kubernetes"
)

// SecretReference is a locator, never a secret value. Adapters resolve it outside
// development memory, event payloads and artifact provenance.
type SecretReference struct{ Store, Key, Version string }
type ProviderExternalScope struct {
	Account, Organization, Project, Region string
	ResourceIDs                            []string
}
type ProviderConnection struct {
	Adapter              string // e.g. github, gitlab, generic-oci; distinct from capability kind
	ExternalScope        ProviderExternalScope
	ID, InstanceID, Name string
	Kind                 ProviderKind
	Endpoint             string
	Credential           SecretReference
}

// A connection existing at instance scope grants no product access by itself.
type ProductConnectionGrant struct {
	ProductID, ConnectionID string
	Read, Mutate            bool
}
type ProviderBinding struct {
	Purpose                  ProviderKind
	ConnectionID, ResourceID string
}

// Purpose is explicit product configuration, never inferred from an environment name.
type ProductEnvironmentConfig struct {
	EnvironmentID, Purpose string
	ComponentIDs           []string
}
type ProductTopology struct {
	EnvironmentConfigs []ProductEnvironmentConfig
	ProductID          string
	Components         []Component
	ComponentConfigs   []ProductComponentConfig
	Bindings           []ProviderBinding
	GitOps             []GitOpsMapping
	Policy             LifecyclePolicy
}
type ProductComponentConfig struct {
	ComponentID, RepositoryID, MainBranch, BuildDefinitionID string
	DependsOn                                                []string
	BuildContextPath                                         string
}

// BranchState pins both comparison ends. Known=false means ahead/behind and
// ancestry are unavailable, never that the branch is synchronized.
type BranchState struct {
	ProductID, RepositoryID, Branch, HeadCommit, MainBranch, MainCommit, BaseCommit string
	Ahead, Behind                                                                   int
	Known, FastForwardAncestryVerified                                              bool
	ObservedAt                                                                      time.Time
}

// FastForwardEligible refers to moving this branch to the pinned main commit.
// Authorization, unchanged heads and provider verification remain required.
func (b BranchState) FastForwardEligible() bool {
	return b.Known && b.FastForwardAncestryVerified && b.Ahead == 0 && b.Behind > 0 && b.HeadCommit != "" && b.MainCommit != "" && !b.ObservedAt.IsZero()
}

type GitOperationKind string

const (
	GitFastForward GitOperationKind = "fast_forward"
	GitMerge       GitOperationKind = "merge"
	GitRebase      GitOperationKind = "rebase"
	GitForceUpdate GitOperationKind = "force_update"
	GitCreate      GitOperationKind = "create"
	GitDelete      GitOperationKind = "delete"
)

type ConflictState string

const (
	ConflictDetected             ConflictState = "detected"
	ConflictRequiresResolution   ConflictState = "requires_resolution"
	ConflictClaimed              ConflictState = "claimed"
	ConflictVerificationRequired ConflictState = "verification_required"
	ConflictResolved             ConflictState = "resolved"
)

type ConflictSide struct{ RevisionID, IntegrationID, ComponentID, RepositoryID, BaseCommit, HeadCommit string }
type ConflictHunkVersion struct{ RevisionID, Commit, Text string }
type ConflictHunk struct {
	Path, BaseCommit, Base string
	Versions               []ConflictHunkVersion
}
type ConflictSemanticSnapshot struct {
	Blockers               []Memory
	Features               []Feature
	Integrations           []Integration
	Decisions, Discoveries []Memory
	Checks                 []Check
	Results                []CheckResult
	Hunks                  []ConflictHunk
}
type ConflictResolution struct {
	Actor, Commit, Rationale string
	RecordedAt               time.Time
	VerificationResultIDs    []string
	VerifiedCommit           string
	VerifiedAt               *time.Time
}
type IntegrationConflict struct {
	ConflictingPaths                           []string
	ID, ProductID, CompositionID, RepositoryID string
	Sides                                      []ConflictSide
	Context                                    ConflictSemanticSnapshot
	State                                      ConflictState
	ClaimedBy                                  string
	ClaimedAt                                  *time.Time
	Resolution                                 *ConflictResolution
	CreatedAt                                  time.Time
}

type ArtifactAvailability string

const (
	ArtifactUnknown ArtifactAvailability = "unknown"
	ArtifactPresent ArtifactAvailability = "present"
	ArtifactMissing ArtifactAvailability = "missing"
	ArtifactDeleted ArtifactAvailability = "deleted"
)

type DigestReference struct{ Name, Digest string }

// Immutable provenance captures the original build, not a recipe that silently
// follows newer branches, lockfiles, base-image tags or credential versions.
type ArtifactProvenance struct {
	ProductID, ComponentID, RepositoryID, SourceCommit, CompositionID              string
	BuildDefinitionID, BuildDefinitionCommit, CIConnectionID, Workflow, BuildRunID string
	Parameters                                                                     map[string]string
	SecretReferences                                                               []SecretReference
	BaseImages, Lockfiles                                                          []DigestReference
	OriginalDigest                                                                 string
}
type ArtifactObservation struct {
	Availability         ArtifactAvailability
	ObservedAt           time.Time
	ConnectionID, Detail string
}
type RebuildGuarantee string

const (
	RebuildUnassessed        RebuildGuarantee = "unassessed"
	RebuildPossible          RebuildGuarantee = "rebuildable"
	RebuildBitForBitVerified RebuildGuarantee = "bit_for_bit_verified"
)

type RebuildAssessment struct {
	Guarantee          RebuildGuarantee
	AssessedAt         time.Time
	EvidenceReferences []string
}

// A rebuild is a new record linked through RebuiltFromArtifactID. It must never
// inherit the original's digest or availability without fresh evidence.
type ArtifactRecord struct {
	ID, URI, Digest       string
	Provenance            ArtifactProvenance
	RebuiltFromArtifactID string
	CreatedAt             time.Time
	Observation           ArtifactObservation
	Rebuild               RebuildAssessment
}

var digestIdentity = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (a ArtifactRecord) ValidDigestIdentity() bool {
	return digestIdentity.MatchString(a.Digest) && a.Provenance.OriginalDigest == a.Digest
}
func (a ArtifactRecord) CurrentAvailability() ArtifactAvailability {
	if a.Observation.ObservedAt.IsZero() {
		return ArtifactUnknown
	}
	switch a.Observation.Availability {
	case ArtifactPresent, ArtifactMissing, ArtifactDeleted:
		return a.Observation.Availability
	}
	return ArtifactUnknown
}

type BuildState string

const (
	BuildQueued    BuildState = "queued"
	BuildRunning   BuildState = "running"
	BuildSucceeded BuildState = "succeeded"
	BuildFailed    BuildState = "failed"
	BuildCancelled BuildState = "cancelled"
)

type BuildRun struct {
	ID, ProductID, ComponentID, ConnectionID, ProviderRunID, OperationID  string
	DefinitionID, DefinitionCommit, Workflow, SourceCommit, CompositionID string
	Parameters                                                            map[string]string
	Secrets                                                               []SecretReference
	State                                                                 BuildState
	ArtifactIDs                                                           []string
	StartedAt, FinishedAt                                                 *time.Time
	EvidenceReferences                                                    []string
}

type DeploymentState string

const (
	DeploymentPlanned            DeploymentState = "planned"
	DeploymentWaitingForArtifact DeploymentState = "waiting_for_artifact"
	DeploymentBuilding           DeploymentState = "building"
	DeploymentArtifactReady      DeploymentState = "artifact_ready"
	DeploymentGitOpsPending      DeploymentState = "gitops_pending"
	DeploymentReconciling        DeploymentState = "reconciling"
	DeploymentVerifying          DeploymentState = "verifying"
	DeploymentDeployed           DeploymentState = "deployed"
	DeploymentFailed             DeploymentState = "failed"
	DeploymentBlocked            DeploymentState = "blocked"
	DeploymentCancelled          DeploymentState = "cancelled"
)

// Evidence pins the artifact and configuration that Flux/runtime actually saw;
// a green cluster or a passing old check alone cannot prove this deployment.
type DeploymentEvidence struct {
	ArtifactDigest, GitOpsCommit, FluxObservedCommit, RuntimeDigest, VerifiedDigest string
	ArtifactObservedAt, FluxObservedAt, RuntimeObservedAt, VerifiedAt               time.Time
	ArtifactAvailability                                                            ArtifactAvailability
	FluxReady, RuntimeHealthy                                                       bool
	VerificationResultIDs                                                           []string
}

func validEvidenceIDs(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (e DeploymentEvidence) ValidForDeployment() bool {
	return digestIdentity.MatchString(e.ArtifactDigest) && e.ArtifactAvailability == ArtifactPresent && !e.ArtifactObservedAt.IsZero() && e.GitOpsCommit != "" && e.FluxObservedCommit == e.GitOpsCommit && e.FluxReady && !e.FluxObservedAt.IsZero() && e.RuntimeDigest == e.ArtifactDigest && e.RuntimeHealthy && !e.RuntimeObservedAt.IsZero() && e.VerifiedDigest == e.ArtifactDigest && validEvidenceIDs(e.VerificationResultIDs) && !e.VerifiedAt.IsZero()
}

type ComponentDeploymentOutcome struct {
	ComponentID, ArtifactID string
	State                   DeploymentState
	Evidence                DeploymentEvidence
	FailureReason           string
}
type DeploymentRun struct {
	// Frozen from the selected composition, never supplied by a completion caller.
	RequiredComponentIDs                                     []string
	ID, ProductID, EnvironmentID, CompositionID, OperationID string
	State                                                    DeploymentState
	Components                                               []ComponentDeploymentOutcome
	CreatedAt                                                time.Time
	FinishedAt                                               *time.Time
}

// ValidateDeployedTransition guards only the final verifying→deployed step; it
// is not a generic workflow engine or proof that earlier steps were executed.
func (d DeploymentRun) ValidateDeployedTransition() error {
	if d.State != DeploymentVerifying || len(d.RequiredComponentIDs) == 0 || len(d.Components) != len(d.RequiredComponentIDs) {
		return errors.New("deployment must be verifying with component outcomes")
	}
	required := map[string]bool{}
	for _, id := range d.RequiredComponentIDs {
		if id == "" || required[id] {
			return errors.New("required component IDs must be nonempty and unique")
		}
		required[id] = true
	}
	seen := map[string]bool{}
	for _, c := range d.Components {
		if !required[c.ComponentID] || c.ComponentID == "" || c.ArtifactID == "" || seen[c.ComponentID] || (c.State != DeploymentVerifying && c.State != DeploymentDeployed) || !c.Evidence.ValidForDeployment() {
			return errors.New("every unique component requires pinned artifact, GitOps, Flux, runtime and verification evidence")
		}
		seen[c.ComponentID] = true
	}
	return nil
}

// ReleaseRecord is final immutable fact, unlike legacy Release (planned intent).
// Creation will require a verified deployed run and frozen component evidence.
type ReleaseRecord struct {
	ID, ProductID, EnvironmentID, DeploymentRunID, CompositionID string
	IntegrationRevisionIDs                                       []string
	Components                                                   []ComponentDeploymentOutcome
	ReleasedAt                                                   time.Time
	RecordedBy                                                   string
}

type ImageFieldMapping struct{ ComponentID, ManifestPath, ImageFieldPath, DigestFieldPath string }
type GitOpsMapping struct {
	ProductID, EnvironmentID, ConnectionID, RepositoryID, Branch, RootPath string
	Images                                                                 []ImageFieldMapping
	FluxConnectionID, FluxNamespace, FluxResourceKind, FluxResourceName    string
}
type BranchPolicy struct {
	AllowAutomaticFastForward, AllowAutomaticMerge, AllowRebase, AllowForceUpdate, AllowDelete bool
	GeneratedPrefix                                                                            string
}
type BuildPolicy struct{ RebuildMissingArtifact, RequirePinnedInputs, RequireDigest bool }
type DeploymentPolicy struct{ AllowAutomaticNonProduction, AllowAutomaticProduction, RequireApproval, RequireFluxReady, RequireRuntimeHealthy, RequireVerification bool }
type LifecyclePolicy struct {
	Branch     BranchPolicy
	Build      BuildPolicy
	Deployment DeploymentPolicy
}

func ConservativeLifecyclePolicy() LifecyclePolicy {
	return LifecyclePolicy{Branch: BranchPolicy{GeneratedPrefix: "generated/"}, Build: BuildPolicy{RequirePinnedInputs: true, RequireDigest: true}, Deployment: DeploymentPolicy{RequireApproval: true, RequireFluxReady: true, RequireRuntimeHealthy: true, RequireVerification: true}}
}

// RebuildReplacementApproval is required when a rebuild produces a different
// digest. The old request is immutable: publish a new plan revision and obtain
// explicit approval rather than silently substituting the rebuilt artifact.
type RebuildReplacementApproval struct {
	ProductID, OriginalArtifactID, RebuiltArtifactID, RequestedDigest, RebuiltDigest, PreviousPlanID, ReplacementPlanID, ApprovedBy string
	ApprovedAt                                                                                                                      time.Time
}

func (r RebuildReplacementApproval) ValidReplacement() bool {
	return digestIdentity.MatchString(r.RequestedDigest) && digestIdentity.MatchString(r.RebuiltDigest) && r.RequestedDigest != r.RebuiltDigest && r.ProductID != "" && r.OriginalArtifactID != "" && r.RebuiltArtifactID != "" && r.OriginalArtifactID != r.RebuiltArtifactID && r.PreviousPlanID != "" && r.ReplacementPlanID != "" && r.PreviousPlanID != r.ReplacementPlanID && r.ApprovedBy != "" && !r.ApprovedAt.IsZero()
}

type WorkClaim struct {
	ID, ProductID, IntegrationID, Owner, RepositoryID, Branch string
	WorkingAreas                                              []string
	ClaimedAt                                                 time.Time
	ReleasedAt                                                *time.Time
}
type AttentionReason string

const (
	AttentionBranchOutdated       AttentionReason = "BRANCH_OUTDATED"
	AttentionMergeConflict        AttentionReason = "MERGE_CONFLICT"
	AttentionArtifactMissing      AttentionReason = "ARTIFACT_MISSING"
	AttentionVerificationRequired AttentionReason = "VERIFICATION_REQUIRED"
)

type ScopedEntityReference struct{ ProductID, Kind, ID string }
type AttentionItem struct {
	ID, Severity       string
	Reason             AttentionReason
	Entity             ScopedEntityReference
	EvidenceReferences []string
	NextAction         string
	ObservedAt         time.Time
}
type RetentionScopeKind string

const (
	RetentionProduct     RetentionScopeKind = "product"
	RetentionComponent   RetentionScopeKind = "component"
	RetentionEnvironment RetentionScopeKind = "environment"
	RetentionConnection  RetentionScopeKind = "connection"
)

// RetentionPolicy is declarative only; no evaluator or cleanup job exists.
// Connection-scoped retention still requires explicit product grants.
type RetentionPolicy struct {
	ProductID                  string
	ScopeKind                  RetentionScopeKind
	ScopeID                    string
	KeepLast, ReleasedKeepLast int
	KeepCurrent                bool
	KeepPrevious               int
	FailedTTL                  time.Duration
}

// ValidateResolvedTransition requires passing results against the resolution
// commit. Verification evidence is supplied explicitly, not inferred from the
// older semantic context captured when the conflict was detected.
func (c IntegrationConflict) ValidateResolvedTransition(results []CheckResult) error {
	r := c.Resolution
	if c.State != ConflictVerificationRequired || r == nil || r.Actor == "" || r.Commit == "" || r.VerifiedCommit != r.Commit || r.VerifiedAt == nil || r.VerifiedAt.IsZero() || !validEvidenceIDs(r.VerificationResultIDs) {
		return errors.New("conflict resolution requires attributed commit and verification")
	}
	for _, id := range r.VerificationResultIDs {
		found := false
		for _, result := range results {
			if result.ID == id && result.ProductID == c.ProductID && result.Result == "passed" && result.Commit == r.Commit {
				found = true
				break
			}
		}
		if !found {
			return errors.New("resolution check must pass on the exact resolution commit")
		}
	}
	return nil
}
