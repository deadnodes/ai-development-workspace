package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
)

type fakeGitOpsPR struct {
	*fakeDelivery
	merged    bool
	closed    bool
	proposals int
}

func (f *fakeGitOpsPR) ProposeGitOps(context.Context, delivery.Connection, delivery.GitOpsApply) (delivery.GitOpsPullRequest, error) {
	f.proposals++
	return delivery.GitOpsPullRequest{Number: 3, URL: "https://github.test/pr/3", HeadSHA: shaHead, State: "open"}, nil
}
func (f *fakeGitOpsPR) ObserveGitOpsPR(_ context.Context, _ delivery.Connection, _ delivery.GitOpsApply, p delivery.GitOpsPullRequest) (delivery.GitOpsPullRequest, error) {
	p.Merged = f.merged
	if f.closed {
		p.State = "closed"
	}
	return p, nil
}
func TestGitOpsPRWaitsForMergeAndExactDigest(t *testing.T) {
	for _, mode := range []string{"merge", "closed", "wrong-image"} {
		t.Run(mode, func(t *testing.T) {
			s, m, base := externalFixture(t)
			f := &fakeGitOpsPR{fakeDelivery: base}
			s.provider = f
			exec(t, s, domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			exec(t, s, deployCommand())
			for n := 0; n < 10 && m.state.Operations[0].Phase != "WAIT_GITOPS_PR"; n++ {
				tickNow(t, s, m)
			}
			op := m.state.Operations[0]
			if op.Phase != "WAIT_GITOPS_PR" || op.Status != "RUNNING" || op.GitOpsResult != nil || f.applies != 0 || f.proposals != 1 {
				t.Fatalf("premature deployment: %+v", op)
			}
			tickNow(t, s, m)
			if m.state.Operations[0].Status != "RUNNING" {
				t.Fatal("did not await reviewer")
			}
			f.merged = mode != "closed"
			f.closed = mode == "closed"
			if mode == "merge" {
				f.currentDigest = op.Artifact.Digest
			}
			tickNow(t, s, m)
			op = m.state.Operations[0]
			if mode == "merge" {
				if op.DeploymentState != "GITOPS_APPLIED" || op.GitOpsResult == nil {
					t.Fatal(op)
				}
			} else if op.Status == "SUCCEEDED" {
				t.Fatal("unverified PR reported applied")
			}
			if f.applies != 0 {
				t.Fatal("direct mutation escaped PR mode")
			}
		})
	}
}
