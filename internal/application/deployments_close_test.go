package application

import (
	"context"
	"errors"
	"reflect"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/github"
	"strings"
	"testing"
	"time"
)

func prepareReconcile(t *testing.T, status string) (*Service, *memoryStore, *deploymentFake, domain.Deployment) {
	t.Helper()
	s, m, f, in := deploymentFixture(t)
	d, err := s.DeployBuild(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	d.Status = status
	d.PRNumber = 42
	d.HeadSHA = strings.Repeat("b", 40)
	d.ProposalBaseSHA = strings.Repeat("a", 40)
	d.UncertainAction = "merge"
	m.state.Deployments[0] = d
	f.pr.Number, f.pr.State = 42, "open"
	f.pr.Head.SHA, f.pr.Head.Ref, f.pr.Head.Repo.FullName = d.HeadSHA, d.Branch, "o/flux"
	f.pr.Base.Ref, f.pr.Base.Repo.FullName = "main", "o/flux"
	return s, m, f, d
}

func TestReconcileDeploymentAdoptsMergedPR(t *testing.T) {
	s, m, f, d := prepareReconcile(t, "MERGE_UNCERTAIN")
	f.pr.Merged, f.pr.MergeSHA = true, strings.Repeat("c", 40)
	out, err := s.ReconcileDeployment(context.Background(), domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "recover merged deployment"})
	if err != nil || out.Resolution != "merged" || out.Deployment.Status != "SUCCEEDED" || out.Deployment.MergeCommitSHA != f.pr.MergeSHA {
		t.Fatalf("%+v %v", out, err)
	}
	if m.state.Deployments[0].Status != "SUCCEEDED" {
		t.Fatal(m.state.Deployments[0])
	}
}

func TestReconcileDeploymentAppliedOnMainClosesPRAndReleasesLock(t *testing.T) {
	s, m, f, d := prepareReconcile(t, "MERGE_UNCERTAIN")
	f.appliedOnMain = true
	out, err := s.ReconcileDeployment(context.Background(), domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "recover applied deployment"})
	if err != nil || out.Resolution != "applied_on_main" || out.Deployment.Status != "SUCCEEDED" || !f.closedPR {
		t.Fatalf("%+v %v closed=%v", out, err, f.closedPR)
	}
	if _, err = s.DeployBuild(context.Background(), domain.DeployBuildRequest{ProductID: "p", BuildID: "build", EnvironmentID: "dev"}); err != nil {
		t.Fatalf("retry admission: %v", err)
	}
	if len(m.state.Deployments) != 2 {
		t.Fatal(len(m.state.Deployments))
	}
}

func TestReconcileDeploymentClosesPRAndBranch(t *testing.T) {
	s, _, f, d := prepareReconcile(t, "RECONCILIATION_REQUIRED")
	out, err := s.ReconcileDeployment(context.Background(), domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "abandon stuck deployment"})
	if err != nil || out.Resolution != "closed" || out.Deployment.Status != "FAILED" || out.Deployment.FailureCode != "OPERATOR_CLOSED" || !f.closedPR || !f.deletedBranch {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err = s.DeployBuild(context.Background(), domain.DeployBuildRequest{ProductID: "p", BuildID: "build", EnvironmentID: "dev"}); err != nil {
		t.Fatalf("retry admission after close: %v", err)
	}
}

func TestReconcileDeploymentProviderErrorLeavesState(t *testing.T) {
	s, m, f, d := prepareReconcile(t, "RECONCILIATION_REQUIRED")
	f.applyErr = errors.New("provider unavailable")
	before := m.state.Deployments[0]
	_, err := s.ReconcileDeployment(context.Background(), domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "retry provider resolution"})
	if err == nil || !reflect.DeepEqual(before, m.state.Deployments[0]) {
		t.Fatalf("changed on provider error: %v", err)
	}
}

func TestReconcileDeploymentRechecksAfterCloseRace(t *testing.T) {
	s, _, f, d := prepareReconcile(t, "RECONCILIATION_REQUIRED")
	f.closePRErr = &github.APIError{Status: 409, Operation: "already merged"}
	f.mergeOnCloseRace = true
	out, err := s.ReconcileDeployment(context.Background(), domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "recover merge race"})
	if err != nil || out.Resolution != "merged" || out.Deployment.Status != "SUCCEEDED" || out.Deployment.MergeCommitSHA != f.pr.MergeSHA {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestReconcileDeploymentRequestKeyIdempotency(t *testing.T) {
	s, _, _, d := prepareReconcile(t, "RECONCILIATION_REQUIRED")
	req := domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "close stuck deployment", RequestKey: "rk"}
	first, err := s.ReconcileDeployment(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReconcileDeployment(context.Background(), req)
	if err != nil || second.Resolution != first.Resolution {
		t.Fatalf("%+v %v", second, err)
	}
	req.Reason = "different reason"
	if _, err = s.ReconcileDeployment(context.Background(), req); err == nil || !strings.Contains(err.Error(), "REQUEST_KEY_CONFLICT") {
		t.Fatal(err)
	}
}

func TestReconcileDeploymentRequiresAdmin(t *testing.T) {
	s, _, _, d := prepareReconcile(t, "RECONCILIATION_REQUIRED")
	writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "writer", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	if _, err := s.ReconcileDeployment(writer, domain.ReconcileDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "reconcile stale deployment"}); err == nil {
		t.Fatal("non-admin reconciled deployment")
	}
}

func TestDeploymentWatchdogThresholdAndStatusTimer(t *testing.T) {
	for _, age := range []time.Duration{2*time.Hour + time.Minute, 2*time.Hour - time.Minute} {
		s, m, f, in := deploymentFixture(t)
		d, err := s.DeployBuild(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		d.Status = "RECONCILIATION_REQUIRED"
		d.UpdatedAt = time.Now().Add(-age)
		d.StatusChangedAt = time.Now().Add(-age)
		d.PRNumber = 0
		m.state.Deployments[0] = d
		if age > 2*time.Hour {
			f.findErr = github.ErrNotFound
		}
		deployTick(t, s)
		got := m.state.Deployments[0]
		if age > 2*time.Hour && (got.Status != "FAILED" || got.FailureCode != "STALE_TIMEOUT") {
			t.Fatalf("stale not closed: %+v", got)
		}
		if age > 2*time.Hour {
			ev := m.state.Events[len(m.state.Events)-1]
			if ev.Action != "stale_close_deployment" || ev.Actor != "system/stale-watchdog" || ev.Data.Data["previous_status"] != "RECONCILIATION_REQUIRED" || ev.Data.Data["resolution"] != "closed" {
				t.Fatalf("incorrect stale audit: %+v", ev)
			}
		}
		if age < 2*time.Hour && got.Status != "RECONCILIATION_REQUIRED" {
			t.Fatalf("fresh changed: %+v", got)
		}
	}
	old := domain.Deployment{Status: "RUNNING", StatusChangedAt: time.Now().Add(-3 * time.Hour)}
	old.Status = "FAILED"
	if !old.StatusChangedAt.Before(time.Now().Add(-2 * time.Hour)) {
		t.Fatal("test setup")
	}
}

func TestCloseDeployment(t *testing.T) {
	ctx := context.Background()
	s, m, f, in := deploymentFixture(t)
	d, err := s.DeployBuild(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	b := m.state.Builds[0]
	b.ID = "another"
	b.SourceSHA = strings.Repeat("b", 40)
	b.Images = append([]domain.BuildImage(nil), b.Images...)
	b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
	b.Images[0].ImageRef = "registry.example/o/r:main-bbbbbbbb@" + b.Images[0].Digest
	m.state.Builds = append(m.state.Builds, b)
	in.BuildID = b.ID
	if _, err := s.DeployBuild(ctx, in); err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS") {
		t.Fatal("active deployment did not block", err)
	}
	req := domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "  abandoned by operator  ", RequestKey: "close"}
	closed, err := s.CloseDeployment(ctx, req)
	if err != nil || closed.Status != "FAILED" || closed.FailureCode != "OPERATOR_CLOSED" || closed.CloseReason != "abandoned by operator" || closed.FinishedAt == nil {
		t.Fatal(closed, err)
	}
	event := m.state.Events[len(m.state.Events)-1]
	data := event.Data.Data
	if event.Action != "close_deployment" || data["reason"] != closed.CloseReason || data["request_key"] != "close" || data["previous_status"] != d.Status || data["previous_failure_code"] != d.FailureCode || data["pr_number"] != 0 {
		t.Fatal(event)
	}
	count := len(m.state.Events)
	replay, err := s.CloseDeployment(ctx, req)
	if err != nil || !reflect.DeepEqual(replay, closed) || len(m.state.Events) != count {
		t.Fatal(replay, err)
	}
	stale := d
	stale.Status = "PROPOSING"
	if err = s.advanceDeployment(ctx, d, &stale, true); err == nil {
		t.Fatal("stale write accepted")
	}
	if err = s.advanceDeployment(ctx, closed, &stale, true); err == nil {
		t.Fatal("terminal write accepted")
	}
	deployTick(t, s)
	if f.proposals != 0 || f.merges != 0 || !reflect.DeepEqual(m.state.Deployments[0], closed) {
		t.Fatal("dispatcher changed closure")
	}
	// A stale PENDING_CHECKS read must fail its CAS before a proposal.
	p := s.builds.config.Profiles[0]
	g := s.builds.config.GitOps[0]
	if err = s.processDeployment(ctx, d, p, g); err != nil {
		t.Fatal(err)
	}
	if f.proposals != 0 {
		t.Fatal("stale reader proposed")
	}
	next, err := s.DeployBuild(ctx, in)
	if err != nil {
		t.Fatal("admission still blocked", err)
	}
	req.DeploymentID = next.ID
	if _, err = s.CloseDeployment(ctx, req); err == nil || !strings.Contains(err.Error(), "REQUEST_KEY_CONFLICT") {
		t.Fatal(err)
	}
}

func TestCloseDeploymentValidation(t *testing.T) {
	for _, tc := range []struct {
		status    string
		pr        int
		uncertain string
		want      string
	}{
		{"PENDING_CHECKS", 0, "", ""}, {"PROPOSING", 0, "", "GitOps side effects"}, {"PROPOSING", 1, "", "GitOps side effects"}, {"MERGING", 0, "", "GitOps side effects"}, {"RECONCILIATION_REQUIRED", 0, "propose", "GitOps side effects"}, {"FAILED", 0, "", "only active"}, {"SUCCEEDED", 0, "", "only active"},
	} {
		t.Run(tc.status+tc.uncertain+string(rune('0'+tc.pr)), func(t *testing.T) {
			s, m, _, in := deploymentFixture(t)
			d, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			m.state.Deployments[0].Status = tc.status
			m.state.Deployments[0].PRNumber = tc.pr
			m.state.Deployments[0].UncertainAction = tc.uncertain
			_, err = s.CloseDeployment(context.Background(), domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "valid reason"})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatal(err)
			}
		})
	}
	s, _, _, in := deploymentFixture(t)
	d, _ := s.DeployBuild(context.Background(), in)
	for _, reason := range []string{"short", strings.Repeat("x", 501)} {
		if _, err := s.CloseDeployment(context.Background(), domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: reason}); err == nil {
			t.Fatal("invalid reason accepted")
		}
	}
	writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "writer", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	if _, err := s.CloseDeployment(writer, domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "valid reason"}); err == nil {
		t.Fatal("non-admin accepted")
	}
}

func TestCloseDeploymentBeforeProposalClaim(t *testing.T) {
	for _, status := range []string{"PENDING_CHECKS"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			s, m, f, in := deploymentFixture(t)
			d, err := s.DeployBuild(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			d.Status = status
			m.state.Deployments[0] = d
			s.store = &closeRaceStore{memoryStore: m, before: func() {
				if _, err := s.CloseDeployment(ctx, domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "abandoned before proposal"}); err != nil {
					t.Fatal(err)
				}
			}}
			_ = s.processDeployment(ctx, d, s.builds.config.Profiles[0], s.builds.config.GitOps[0])
			if f.proposals != 0 || f.merges != 0 || m.state.Deployments[0].FailureCode != "OPERATOR_CLOSED" {
				t.Fatal("proposal raced closure")
			}
		})
	}
}
