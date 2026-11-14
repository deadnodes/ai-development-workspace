package application

import (
	"context"
	"reflect"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

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
