package application

import (
	"context"
	"errors"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"releasecontrol/internal/providers/github"
	"strings"
	"testing"
	"time"
)

type deploymentFake struct {
	status                           forgejo.CommitStatus
	registryErr                      error
	proposals, merges, heads         int
	proposalErr, mergeErr, verifyErr error
	pr                               github.DeployPR
	already                          bool
	plan                             github.DeployPlan
}

func (f *deploymentFake) CommitStatus(context.Context, buildconfig.Connection, string, string) (forgejo.CommitStatus, error) {
	return f.status, nil
}
func (f *deploymentFake) HeadDigest(context.Context, buildconfig.Registry, string, string) error {
	f.heads++
	return f.registryErr
}
func (f *deploymentFake) ProposeDeployment(_ context.Context, g buildconfig.GitOps, d github.DeployPlan) (github.DeployProposal, error) {
	f.proposals++
	f.plan = d
	f.pr.Number = 1
	f.pr.URL = "https://github.example/pr/1"
	f.pr.Head.SHA = strings.Repeat("b", 40)
	f.pr.Head.Ref = d.Branch
	f.pr.Head.Repo.FullName = g.Repository
	f.pr.Base.Ref = "main"
	f.pr.Base.SHA = strings.Repeat("c", 40)
	f.pr.Base.Repo.FullName = g.Repository
	f.pr.State = "open"
	return github.DeployProposal{PR: f.pr, BaseSHA: f.pr.Base.SHA, HeadSHA: f.pr.Head.SHA, AlreadyApplied: f.already}, f.proposalErr
}
func (f *deploymentFake) FindDeployment(context.Context, buildconfig.GitOps, string) (github.DeployPR, error) {
	return f.pr, nil
}
func (f *deploymentFake) ReadDeployment(context.Context, buildconfig.GitOps, int) (github.DeployPR, error) {
	return f.pr, nil
}
func (f *deploymentFake) VerifyDeployment(context.Context, buildconfig.GitOps, github.DeployPlan, github.DeployPR) error {
	return f.verifyErr
}
func (f *deploymentFake) MergeDeployment(_ context.Context, _ buildconfig.GitOps, _ int, sha string) (string, error) {
	f.merges++
	if sha != f.pr.Head.SHA {
		panic("missing SHA lock")
	}
	return strings.Repeat("d", 40), f.mergeErr
}
func deploymentFixture(t *testing.T) (*Service, *memoryStore, *deploymentFake, domain.DeployBuildRequest) {
	t.Helper()
	s, m, _, _ := buildFixture(t)
	c := buildConfig()
	c.GitOps = []buildconfig.GitOps{{ID: "flux", Kind: "github", APIURL: "https://api.github.com", AppID: 1, InstallationID: 2, PrivateKeyRef: "github.pem", Repository: "o/flux", Branch: "main"}}
	c.Environments = []buildconfig.Environment{{ID: "dev", GitOpsID: "flux", AllowAutoMerge: true}}
	c.Profiles[0].Deployments = map[string]buildconfig.DeploymentTarget{"dev": {Files: []buildconfig.DeploymentFile{{Path: "clusters/dev/app.yaml", ImageID: "app", ImageRepository: "registry.example/o/r", TagFormat: "{tag}@{digest}", MinMatches: 1}}}}
	c.Profiles[0].RequiredStatusContexts = []string{"verify"}
	if e := s.ConfigureBuilds(c, &fakeForgejo{}, &fakeBuildRegistry{}); e != nil {
		t.Fatal(e)
	}
	f := &deploymentFake{status: forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "success"}}}}
	if e := s.ConfigureDeployments(f, f, f); e != nil {
		t.Fatal(e)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	m.state.Builds = []domain.Build{{Meta: domain.Meta{ID: "build", ProductID: "p"}, Status: "SUCCEEDED", AuthorityHash: s.builds.hash, ProfileID: "app", SourceSHA: strings.Repeat("a", 40), Images: []domain.BuildImage{{ID: "app", Digest: digest, ImageRef: "registry.example/o/r:main-aaaaaaaa@" + digest}}}}
	return s, m, f, domain.DeployBuildRequest{ProductID: "p", BuildID: "build", EnvironmentID: "dev"}
}
func deployTick(t *testing.T, s *Service) {
	t.Helper()
	if e := s.DispatchDeploymentsOnce(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestDeployBuildGatesAndCI(t *testing.T) {
	for _, tc := range []struct{ name, state, context, registry, want string }{
		{"pending", "pending", "pending", "", "PENDING_CHECKS"}, {"failure", "failure", "failure", "", "FAILED"}, {"missing context", "success", "", "", "PENDING_CHECKS"}, {"combined success", "success", "none", "", "SUCCEEDED"}, {"combined failure", "error", "none", "", "FAILED"}, {"digest missing", "success", "success", "missing", "FAILED"}, {"green", "success", "success", "", "SUCCEEDED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			f.status.State = tc.state
			f.status.Statuses = nil
			if tc.context == "none" {
				s.builds.config.Profiles[0].RequiredStatusContexts = nil
			} else if tc.context != "" {
				f.status.Statuses = []forgejo.StatusContext{{Context: "verify", Status: tc.context}}
			}
			if tc.registry == "missing" {
				f.registryErr = buildregistry.ErrMissing
			}
			if _, e := s.DeployBuild(context.Background(), in); e != nil {
				t.Fatal(e)
			}
			deployTick(t, s)
			d := m.state.Deployments[0]
			if d.Status != tc.want {
				t.Fatalf("%+v", d)
			}
			if d.Status == "FAILED" && f.proposals != 0 {
				t.Fatal("failed gate proposed")
			}
			if d.Status == "PENDING_CHECKS" {
				f.status = forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "success"}}}
				deployTick(t, s)
				if m.state.Deployments[0].Status != "SUCCEEDED" {
					t.Fatal(m.state.Deployments)
				}
			}
		})
	}
}
func TestDeployIdempotencyScopeAndBody(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	in.RequestKey = "key"
	d, e := s.DeployBuild(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.DeployBuild(context.Background(), in)
	if e != nil || d.ID != again.ID {
		t.Fatal(again, e)
	}
	in.RequestKey = "other"
	again, e = s.DeployBuild(context.Background(), in)
	if e != nil || again.ID != d.ID || len(m.state.Deployments) != 1 {
		t.Fatal(again, e)
	}
	in.BuildID = "missing"
	if _, e = s.DeployBuild(context.Background(), in); e == nil {
		t.Fatal("key rebound")
	}
	in.EnvironmentID = "prod"
	if _, e = s.DeployBuild(context.Background(), in); e == nil {
		t.Fatal("prod accepted")
	}
	deployTick(t, s)
	if f.merges != 1 || f.heads != 1 {
		t.Fatal(f)
	}
	for _, text := range []string{"build", "Source SHA:", "digest sha256:", "CI contexts:", "verify", "Registry checked:"} {
		if !strings.Contains(f.plan.Body, text) {
			t.Fatal(f.plan.Body)
		}
	}
	if _, e = s.GetDeployment(context.Background(), domain.DeploymentLookup{ProductID: "wrong", DeploymentID: d.ID}); e == nil {
		t.Fatal("cross product")
	}
	if _, e = s.ListDeployments(context.Background(), domain.ListDeploymentsRequest{ProductID: "p", Limit: 101}); e == nil {
		t.Fatal("unbounded")
	}
}
func TestDeployAlreadyApplied(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	f.already = true
	s.DeployBuild(context.Background(), in)
	deployTick(t, s)
	d := m.state.Deployments[0]
	if d.Status != "SUCCEEDED" || !d.AlreadyApplied || d.PRNumber != 0 || f.merges != 0 {
		t.Fatal(d, f.merges)
	}
}
func TestDeployConflictRetry(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		s, m, f, in := deploymentFixture(t)
		f.mergeErr = &github.APIError{Status: 409}
		s.DeployBuild(context.Background(), in)
		deployTick(t, s)
		if m.state.Deployments[0].Status != "PROPOSING" {
			t.Fatal(m.state.Deployments)
		}
		if !persistent {
			f.mergeErr = nil
		}
		deployTick(t, s)
		want := "SUCCEEDED"
		if persistent {
			want = "FAILED"
		}
		if m.state.Deployments[0].Status != want || f.proposals != 2 || f.merges != 2 || m.state.Deployments[0].PRNumber != 1 {
			t.Fatal(m.state.Deployments, f)
		}
	}
}
func TestDeployLostResponsesRestart(t *testing.T) {
	for _, action := range []string{"propose", "merge"} {
		t.Run(action, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			if action == "propose" {
				f.proposalErr = errors.New("lost response")
			} else {
				f.mergeErr = errors.New("lost response")
			}
			s.DeployBuild(context.Background(), in)
			deployTick(t, s)
			if m.state.Deployments[0].Status != "RECONCILIATION_REQUIRED" {
				t.Fatal(m.state.Deployments)
			}
			// A new service resumes the same persisted state and adapters.
			fresh := New(m)
			if e := fresh.ConfigureBuilds(s.builds.config, &fakeForgejo{}, &fakeBuildRegistry{}); e != nil {
				t.Fatal(e)
			}
			fresh.ConfigureDeployments(f, f, f)
			if action == "merge" {
				deployTick(t, fresh)
				if f.merges != 1 {
					t.Fatal("blind remerge")
				}
				f.pr.Merged = true
				f.pr.MergeSHA = strings.Repeat("e", 40)
			} else {
				f.proposalErr = nil
			}
			deployTick(t, fresh)
			if m.state.Deployments[0].Status != "SUCCEEDED" || f.proposals != 1 || f.merges != 1 {
				t.Fatal(m.state.Deployments, f)
			}
		})
	}
}
func TestDeployFileSerializationAndDeadline(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	f.mergeErr = errors.New("unknown")
	s.DeployBuild(context.Background(), in)
	deployTick(t, s)
	b := m.state.Builds[0]
	b.ID = "second"
	b.Images = append([]domain.BuildImage(nil), b.Images...)
	b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
	b.Images[0].ImageRef = "registry.example/o/r:main-aaaaaaaa@" + b.Images[0].Digest
	m.state.Builds = append(m.state.Builds, b)
	in.BuildID = b.ID
	s.DeployBuild(context.Background(), in)
	deployTick(t, s)
	if f.proposals != 1 || m.state.Deployments[1].Status != "PENDING_CHECKS" {
		t.Fatal(m.state.Deployments)
	}
	f.status.State = "pending"
	f.status.Statuses = nil
	m.state.Deployments[1].Deadline = time.Now().Add(-time.Second)
	deployTick(t, s)
	if m.state.Deployments[1].FailureCode != "CI_NOT_GREEN" {
		t.Fatal(m.state.Deployments)
	}
}

func TestDeployAuthorization(t *testing.T) {
	s, _, _, in := deploymentFixture(t)
	reader := auth.WithPrincipal(context.Background(), auth.Principal{ID: "reader", Products: []string{"p"}, Permissions: []string{"read"}})
	if _, e := s.DeployBuild(reader, in); e == nil {
		t.Fatal("read permission deployed")
	}
	writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "writer", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	d, e := s.DeployBuild(writer, in)
	if e != nil || d.Actor != "writer" {
		t.Fatal(d, e)
	}
	other := auth.WithPrincipal(context.Background(), auth.Principal{ID: "other", Products: []string{"else"}, Permissions: []string{"read", "write"}})
	if _, e = s.GetDeployment(other, domain.DeploymentLookup{ProductID: "p", DeploymentID: d.ID}); e == nil {
		t.Fatal("cross Product read")
	}
	if _, e = s.ListDeployments(other, domain.ListDeploymentsRequest{ProductID: "p"}); e == nil {
		t.Fatal("cross Product list")
	}
}

func TestDeployAuthorityChangeStopsIntent(t *testing.T) {
	for _, active := range []bool{false, true} {
		s, m, f, in := deploymentFixture(t)
		if active {
			f.mergeErr = errors.New("lost response")
		}
		s.DeployBuild(context.Background(), in)
		if active {
			deployTick(t, s)
		}
		s.builds.hash = "changed"
		deployTick(t, s)
		d := m.state.Deployments[0]
		if d.FailureCode != "DEPLOYMENT_AUTHORITY_CHANGED" {
			t.Fatal(d)
		}
		if active && d.Status != "RECONCILIATION_REQUIRED" || !active && d.Status != "FAILED" {
			t.Fatal(d)
		}
	}
}

func TestDeployListCursorBinding(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	d, e := s.DeployBuild(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"a", "b", "c"} {
		copy := d
		copy.ID = key
		m.state.Deployments = append(m.state.Deployments, copy)
	}
	page, e := s.ListDeployments(context.Background(), domain.ListDeploymentsRequest{ProductID: "p", Limit: 1})
	if e != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatal(page, e)
	}
	next, e := s.ListDeployments(context.Background(), domain.ListDeploymentsRequest{ProductID: "p", Limit: 1, Cursor: page.NextCursor})
	if e != nil || len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Fatal(next, e)
	}
	for _, request := range []domain.ListDeploymentsRequest{{ProductID: "other", Cursor: page.NextCursor}, {ProductID: "p", ProfileID: "other", Cursor: page.NextCursor}, {ProductID: "p", EnvironmentID: "dev", Cursor: page.NextCursor}, {ProductID: "p", Cursor: "garbage"}} {
		if _, e = s.ListDeployments(context.Background(), request); e == nil {
			t.Fatal("cursor rebound", request)
		}
	}
}
