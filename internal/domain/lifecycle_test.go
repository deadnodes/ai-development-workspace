package domain

import (
	"strings"
	"testing"
	"time"
)

func TestFastForwardRequiresKnownNonDivergedAncestry(t *testing.T) {
	branch := BranchState{HeadCommit: "head", MainCommit: "main", Known: true, Ahead: 0, Behind: 2, FastForwardAncestryVerified: true, ObservedAt: time.Now()}
	if !branch.FastForwardEligible() {
		t.Fatal("verified behind-only branch should be eligible")
	}
	branch.Ahead = 1
	if branch.FastForwardEligible() {
		t.Fatal("diverged branch must never fast-forward")
	}
	branch.Ahead = 0
	branch.Known = false
	if branch.FastForwardEligible() {
		t.Fatal("unknown comparison must not fast-forward")
	}
	branch.Known = true
	branch.FastForwardAncestryVerified = false
	if branch.FastForwardEligible() {
		t.Fatal("counts do not prove ancestry")
	}
}
func TestArtifactIdentityDoesNotImplyAvailabilityOrRebuildEquality(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	a := ArtifactRecord{Digest: digest, Provenance: ArtifactProvenance{OriginalDigest: digest}}
	if !a.ValidDigestIdentity() || a.CurrentAvailability() != ArtifactUnknown {
		t.Fatal("identity must not imply presence")
	}
	a.Observation = ArtifactObservation{Availability: ArtifactPresent}
	if a.CurrentAvailability() != ArtifactUnknown {
		t.Fatal("undated observation must be unknown")
	}
	a.Observation.ObservedAt = time.Now()
	if a.CurrentAvailability() != ArtifactPresent {
		t.Fatal("fresh explicit observation missing")
	}
	a.Provenance.OriginalDigest = "sha256:" + strings.Repeat("b", 64)
	if a.ValidDigestIdentity() {
		t.Fatal("mismatched immutable digest accepted")
	}
	if (RebuildReplacementApproval{RequestedDigest: digest, RebuiltDigest: a.Provenance.OriginalDigest}).ValidReplacement() {
		t.Fatal("digest change requires plan revision and approval")
	}
	approval := RebuildReplacementApproval{ProductID: "p", OriginalArtifactID: "old", RebuiltArtifactID: "new", RequestedDigest: digest, RebuiltDigest: a.Provenance.OriginalDigest, PreviousPlanID: "plan1", ReplacementPlanID: "plan2", ApprovedBy: "human", ApprovedAt: time.Now()}
	if !approval.ValidReplacement() {
		t.Fatal("explicit replacement should validate")
	}
	approval.ReplacementPlanID = approval.PreviousPlanID
	if approval.ValidReplacement() {
		t.Fatal("original plan cannot be silently rewritten")
	}
}
func TestDeploymentRequiresEveryComponentEvidence(t *testing.T) {
	now := time.Now()
	digest := "sha256:" + strings.Repeat("a", 64)
	e := DeploymentEvidence{ArtifactDigest: digest, ArtifactAvailability: ArtifactPresent, ArtifactObservedAt: now, GitOpsCommit: "gitops", FluxObservedCommit: "gitops", FluxReady: true, FluxObservedAt: now, RuntimeDigest: digest, RuntimeHealthy: true, RuntimeObservedAt: now, VerifiedDigest: digest, VerificationResultIDs: []string{"check-result"}, VerifiedAt: now}
	run := DeploymentRun{RequiredComponentIDs: []string{"api"}, State: DeploymentVerifying, Components: []ComponentDeploymentOutcome{{ComponentID: "api", ArtifactID: "artifact", State: DeploymentVerifying, Evidence: e}}}
	if err := run.ValidateDeployedTransition(); err != nil {
		t.Fatal(err)
	}
	run.Components[0].Evidence.FluxObservedCommit = "old"
	if run.ValidateDeployedTransition() == nil {
		t.Fatal("old Flux revision cannot prove deployment")
	}
	run.Components[0].Evidence = e
	run.Components[0].Evidence.RuntimeDigest = "sha256:" + strings.Repeat("b", 64)
	if run.ValidateDeployedTransition() == nil {
		t.Fatal("different runtime image cannot prove deployment")
	}
	run.Components[0].Evidence = e
	run.Components = append(run.Components, ComponentDeploymentOutcome{ComponentID: "web", State: DeploymentBlocked})
	if run.ValidateDeployedTransition() == nil {
		t.Fatal("partial deployment cannot be deployed")
	}
	run.Components = run.Components[:1]
	run.RequiredComponentIDs = []string{"api", "web"}
	if run.ValidateDeployedTransition() == nil {
		t.Fatal("missing required component cannot be deployed")
	}
	run.RequiredComponentIDs = []string{"api"}
	run.State = DeploymentPlanned
	if run.ValidateDeployedTransition() == nil {
		t.Fatal("planned cannot skip to deployed")
	}
}
func TestLifecycleDefaultsNeverAuthorizeMutation(t *testing.T) {
	p := ConservativeLifecyclePolicy()
	if p.Branch.AllowAutomaticFastForward || p.Branch.AllowAutomaticMerge || p.Branch.AllowRebase || p.Branch.AllowForceUpdate || p.Branch.AllowDelete || p.Build.RebuildMissingArtifact || p.Deployment.AllowAutomaticNonProduction || p.Deployment.AllowAutomaticProduction {
		t.Fatal("defaults authorize unattended mutation")
	}
	if !p.Build.RequireDigest || !p.Build.RequirePinnedInputs || !p.Deployment.RequireApproval || !p.Deployment.RequireFluxReady || !p.Deployment.RequireRuntimeHealthy || !p.Deployment.RequireVerification {
		t.Fatal("evidence defaults must be conservative")
	}
}

func TestVerificationEvidenceIDsMustBeNonemptyAndUnique(t *testing.T) {
	for _, ids := range [][]string{nil, {""}, {" "}, {"result", "result"}} {
		if validEvidenceIDs(ids) {
			t.Fatalf("accepted invalid evidence IDs %#v", ids)
		}
	}
	if !validEvidenceIDs([]string{"result-a", "result-b"}) {
		t.Fatal("valid evidence IDs rejected")
	}
}
func TestConflictResolutionRequiresChecksOnResolutionCommit(t *testing.T) {
	now := time.Now()
	c := IntegrationConflict{ProductID: "p", State: ConflictVerificationRequired, Resolution: &ConflictResolution{Actor: "agent", Commit: "fix", VerifiedCommit: "fix", VerifiedAt: &now, VerificationResultIDs: []string{"result"}}}
	results := []CheckResult{{Meta: Meta{ID: "result", ProductID: "p"}, Result: "passed", Commit: "fix"}}
	if err := c.ValidateResolvedTransition(results); err != nil {
		t.Fatal(err)
	}
	results[0].Commit = "old"
	if c.ValidateResolvedTransition(results) == nil {
		t.Fatal("old check cannot verify a new resolution")
	}
}
