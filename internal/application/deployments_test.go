package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"releasecontrol/internal/providers/github"
	"strconv"
	"strings"
	"testing"
	"time"
)

type deploymentFake struct {
	ancestor      bool
	ancestorErr   error
	ancestorCalls int
	ancestorHook  func()

	status                           forgejo.CommitStatus
	registryErr                      error
	statusErr                        error
	proposals, merges, heads         int
	proposalErr, mergeErr, verifyErr error
	pr                               github.DeployPR
	already                          bool
	plan                             github.DeployPlan
	advanceBase                      bool
	verifyPlans                      []github.DeployPlan
	content, proposedContent         string
	openedPRs                        int
}

func (f *deploymentFake) IsAncestor(ctx context.Context, cn buildconfig.Connection, repo, ancestor, descendant string) (bool, error) {
	f.ancestorCalls++
	if _, ok := ctx.Deadline(); !ok {
		panic("comparison must be bounded")
	}
	if f.ancestorHook != nil {
		f.ancestorHook()
	}
	return f.ancestor, f.ancestorErr
}
func (f *deploymentFake) CommitStatus(context.Context, buildconfig.Connection, string, string) (forgejo.CommitStatus, error) {
	return f.status, f.statusErr
}
func (f *deploymentFake) HeadDigest(context.Context, buildconfig.Registry, string, string) error {
	f.heads++
	return f.registryErr
}
func (f *deploymentFake) ProposeDeployment(_ context.Context, g buildconfig.GitOps, d github.DeployPlan) (github.DeployProposal, error) {
	f.proposals++
	f.plan = d
	if f.content != "" {
		rendered, err := github.RenderDeployment(f.content, d.Files[0].Tags)
		if err != nil {
			return github.DeployProposal{}, err
		}
		if rendered == f.content {
			return github.DeployProposal{AlreadyApplied: true}, nil
		}
		f.proposedContent = rendered
		f.openedPRs++
	}
	f.pr.Number = 1
	f.pr.URL = "https://github.example/pr/1"
	f.pr.Head.SHA = strings.Repeat("b", 40)
	f.pr.Head.Ref = d.Branch
	f.pr.Head.Repo.FullName = g.Repository
	f.pr.Base.Ref = "main"
	f.pr.Base.SHA = strings.Repeat(fmt.Sprint(f.proposals), 40)
	f.pr.Base.Repo.FullName = g.Repository
	f.pr.State = "open"
	return github.DeployProposal{PR: f.pr, BaseSHA: f.pr.Base.SHA, HeadSHA: f.pr.Head.SHA, AlreadyApplied: f.already}, f.proposalErr
}
func (f *deploymentFake) FindDeployment(context.Context, buildconfig.GitOps, string) (github.DeployPR, error) {
	return f.pr, nil
}
func (f *deploymentFake) ReadDeployment(context.Context, buildconfig.GitOps, int) (github.DeployPR, error) {
	if f.advanceBase {
		f.pr.Base.SHA = strings.Repeat("9", 40)
	}
	return f.pr, nil
}
func (f *deploymentFake) VerifyDeployment(_ context.Context, _ buildconfig.GitOps, plan github.DeployPlan, pr github.DeployPR) error {
	f.verifyPlans = append(f.verifyPlans, plan)
	if plan.ExpectedHead != pr.Head.SHA {
		return github.ErrConflict
	}
	if plan.ProposalBaseSHA == "" {
		return &github.APIError{Status: 409}
	}
	return f.verifyErr
}
func (f *deploymentFake) MergeDeployment(_ context.Context, _ buildconfig.GitOps, _ int, sha string) (string, error) {
	f.merges++
	if sha != f.pr.Head.SHA {
		panic("missing SHA lock")
	}
	if f.mergeErr == nil && f.content != "" {
		f.content = f.proposedContent
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
		f.advanceBase = true
		f.mergeErr = &github.APIError{Status: 409}
		s.DeployBuild(context.Background(), in)
		deployTick(t, s)
		if m.state.Deployments[0].Status != "PROPOSING" {
			t.Fatal(m.state.Deployments)
		}
		if len(f.verifyPlans) != 1 || f.verifyPlans[0].ProposalBaseSHA != strings.Repeat("1", 40) {
			t.Fatal(f.verifyPlans)
		}
		if !persistent {
			f.mergeErr = nil
		}
		deployTick(t, s)
		want := "SUCCEEDED"
		if persistent {
			want = "FAILED"
		}
		if len(f.verifyPlans) != 2 || f.verifyPlans[1].ProposalBaseSHA != strings.Repeat("2", 40) || m.state.Deployments[0].RetryCount != 1 {
			t.Fatal(f.verifyPlans, m.state.Deployments)
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
			if action == "propose" {
				if m.state.Deployments[0].Status != "RECONCILIATION_REQUIRED" || f.proposals != 1 || f.merges != 0 {
					t.Fatal(m.state.Deployments, f)
				}
				return
			}
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
	if _, err := s.DeployBuild(context.Background(), in); err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS:") {
		t.Fatal(err)
	}
	if len(m.state.Deployments) != 1 {
		t.Fatal(m.state.Deployments)
	}
	m.state.Deployments[0].Status = "FAILED"
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	f.status.State = "pending"
	f.status.Statuses = nil
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

func TestDeploymentPlanContainerImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	p := buildconfig.Profile{Deployments: map[string]buildconfig.DeploymentTarget{"dev": {Files: []buildconfig.DeploymentFile{{Path: "workloads.yaml", Format: "container_image", ImageID: "web", ImageRepository: "registry:5000/o/web", TagFormat: "ignored", MinMatches: 2}}}}}
	d := domain.Deployment{Images: []domain.BuildImage{{ID: "web", ImageRef: "registry:5000/o/web:forgejo-12345678@" + digest, Digest: digest}}}
	plan := deploymentPlan(d, p)
	if len(plan.Files) != 1 || len(plan.Files[0].Tags) != 1 {
		t.Fatal(plan)
	}
	tag := plan.Files[0].Tags[0]
	if tag.Format != "container_image" || tag.Value != "forgejo-12345678@"+digest || tag.MinMatches != 2 {
		t.Fatal(tag)
	}
}

func TestDeployStatusPageLimitUnavailable(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	f.statusErr = forgejo.ErrStatusPageLimit
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	m.state.Deployments[0].Deadline = time.Now().Add(-time.Hour)
	deployTick(t, s)
	d := m.state.Deployments[0]
	if d.Status != "PENDING_CHECKS" || d.FailureCode != "CI_UNAVAILABLE" || f.proposals != 0 {
		t.Fatalf("pagination exhaustion must wait: %+v", d)
	}
}

type deployStatusTransport func(*http.Request) (*http.Response, error)

func (f deployStatusTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDeployStatusRequiredContextOnPageTwo(t *testing.T) {
	for _, required := range []bool{false, true} {
		t.Run(fmt.Sprint(required), func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			if !required {
				s.builds.config.Profiles[0].RequiredStatusContexts = nil
			}
			dir := t.TempDir()
			t.Setenv("RCP_SECRET_DIR", dir)
			if err := os.WriteFile(filepath.Join(dir, "token"), []byte("private-token"), 0600); err != nil {
				t.Fatal(err)
			}
			s.builds.config.Connections[0].TokenRef = "token"
			calls := 0
			source := forgejo.NewWithClient(&http.Client{Transport: deployStatusTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Query().Get("page") != strconv.Itoa(calls) || r.URL.Query().Get("limit") != "50" {
					t.Fatal(r.URL)
				}
				v := forgejo.CommitStatus{State: "success", TotalCount: 51}
				if calls == 1 {
					for i := 0; i < 50; i++ {
						v.Statuses = append(v.Statuses, forgejo.StatusContext{Context: fmt.Sprint(i), Status: "success"})
					}
				} else {
					v.Statuses = []forgejo.StatusContext{{Context: "verify", Status: "success"}}
				}
				body, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
			})})
			if err := s.ConfigureDeployments(source, f, f); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DeployBuild(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			deployTick(t, s)
			if calls != 2 || m.state.Deployments[0].Status != "SUCCEEDED" {
				t.Fatalf("calls=%d deployment=%+v", calls, m.state.Deployments[0])
			}
		})
	}
}

func TestDeployMissingProposalBaseAndEditedHead(t *testing.T) {
	for _, edited := range []bool{false, true} {
		t.Run(fmt.Sprint(edited), func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			f.verifyErr = errors.New("wait before merge")
			s.DeployBuild(context.Background(), in)
			deployTick(t, s)
			d := &m.state.Deployments[0]
			if d.ProposalBaseSHA != strings.Repeat("1", 40) {
				t.Fatal(d)
			}
			if edited {
				f.pr.Head.SHA = strings.Repeat("e", 40)
			} else {
				d.ProposalBaseSHA = ""
			}
			f.verifyErr = nil
			deployTick(t, s)
			if edited {
				if m.state.Deployments[0].FailureCode != "GITOPS_SCOPE_CHANGED" || f.merges != 0 {
					t.Fatal(m.state.Deployments, f)
				}
				return
			}
			if m.state.Deployments[0].Status != "PROPOSING" || f.merges != 0 {
				t.Fatal(m.state.Deployments, f)
			}
			deployTick(t, s)
			if m.state.Deployments[0].Status != "SUCCEEDED" || f.proposals != 2 || f.merges != 1 || m.state.Deployments[0].ProposalBaseSHA != strings.Repeat("2", 40) {
				t.Fatal(m.state.Deployments, f)
			}
		})
	}
}

func TestDeployRollbackAndTerminalIdempotency(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyed=%t", keyed), func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			ctx := context.Background()
			f.content = "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nspec:\n  values:\n    image:\n      repository: registry.example/o/r\n      tag: initial\n"
			b := m.state.Builds[0]
			b.ID = "build-b"
			b.Images = append([]domain.BuildImage(nil), b.Images...)
			b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
			b.Images[0].ImageRef = "registry.example/o/r:main-bbbbbbbb@" + b.Images[0].Digest
			m.state.Builds = append(m.state.Builds, b)
			request := func(build, key string) domain.Deployment {
				t.Helper()
				in.BuildID, in.RequestKey = build, ""
				if keyed {
					in.RequestKey = key
				}
				d, err := s.DeployBuild(ctx, in)
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			finish := func(d domain.Deployment) domain.Deployment {
				t.Helper()
				deployTick(t, s)
				got, err := s.GetDeployment(ctx, domain.DeploymentLookup{ProductID: "p", DeploymentID: d.ID})
				if err != nil || got.Status != "SUCCEEDED" {
					t.Fatal(got, err)
				}
				return got
			}
			a := finish(request("build", "a"))
			finish(request("build-b", "b"))
			if !strings.Contains(f.content, b.Images[0].Digest) {
				t.Fatal("B not applied", f.content)
			}
			if keyed {
				if replay := request("build", "a"); replay.ID != a.ID || replay.Status != "SUCCEEDED" || len(m.state.Deployments) != 2 {
					t.Fatal("historical receipt changed desired state", replay)
				}
			}
			rollback := request("build", "rollback")
			if rollback.ID == a.ID {
				t.Fatal("rollback reused historical handle")
			}
			duplicate := request("build", "active-duplicate")
			if duplicate.ID != rollback.ID {
				t.Fatal("active duplicate created another deployment")
			}
			rollback = finish(rollback)
			if rollback.AlreadyApplied || rollback.PRNumber == 0 || f.openedPRs != 3 || f.merges != 3 || !strings.Contains(f.content, a.Images[0].Digest) {
				t.Fatal("rollback did not apply A through a PR", rollback, f.content)
			}
			repeat := request("build", "repeat")
			if repeat.ID == a.ID || repeat.ID == rollback.ID {
				t.Fatal("terminal deployment reused")
			}
			repeat = finish(repeat)
			if !repeat.AlreadyApplied || repeat.PRNumber != 0 || f.openedPRs != 3 || f.merges != 3 {
				t.Fatal("applied repeat opened a PR", repeat)
			}
			if keyed {
				for key, want := range map[string]string{"a": a.ID, "rollback": rollback.ID, "active-duplicate": rollback.ID, "repeat": repeat.ID} {
					if got := request("build", key); got.ID != want || got.Status != "SUCCEEDED" {
						t.Fatal("receipt did not replay historical handle", key, got)
					}
				}
				in.BuildID, in.RequestKey = "build-b", "a"
				if _, err := s.DeployBuild(ctx, in); err == nil || !strings.Contains(err.Error(), "REQUEST_KEY_CONFLICT") {
					t.Fatal("receipt rebound", err)
				}
			}
			if len(m.state.Deployments) != 4 {
				t.Fatal("unexpected deployment count", len(m.state.Deployments))
			}
		})
	}
}

func TestDeployFailedIdentityCreatesNewDeployment(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	first, err := s.DeployBuild(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m.state.Deployments[0].Status = "FAILED"
	retry, err := s.DeployBuild(context.Background(), in)
	if err != nil || retry.ID == first.ID || retry.Status != "PENDING_CHECKS" {
		t.Fatal(retry, err)
	}
}

func TestDeployBuildRollbackGuard(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		previous, same, ancestor, unavailable, allow bool
		reason, code, check                          string
	}{
		{name: "ancestor refused", previous: true, ancestor: true, code: "ROLLBACK_REFUSED:"},
		{name: "ancestor override", previous: true, ancestor: true, allow: true, reason: " owner approved rollback ", check: "ancestor"},
		{name: "missing reason", allow: true, code: "allow_rollback requires reason"},
		{name: "long reason", allow: true, reason: strings.Repeat("я", 501), code: "allow_rollback requires reason"},
		{name: "unicode reason", previous: true, ancestor: true, allow: true, reason: strings.Repeat("я", 8), check: "ancestor"},
		{name: "short trimmed reason", allow: true, reason: "  short  ", code: "allow_rollback requires reason"},
		{name: "reason only", reason: "owner approved rollback", code: "reason requires allow_rollback"},
		{name: "newer or diverged", previous: true},
		{name: "unused override", previous: true, allow: true, reason: "owner approved rollback"},
		{name: "unavailable refused", previous: true, unavailable: true, code: "ROLLBACK_CHECK_UNAVAILABLE:"},
		{name: "unavailable override", previous: true, unavailable: true, allow: true, reason: "owner approved rollback", check: "unavailable"},
		{name: "same SHA", previous: true, same: true, ancestor: true},
		{name: "no previous", ancestor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			f.ancestor, f.ancestorErr = tc.ancestor, nil
			if tc.unavailable {
				f.ancestorErr = errors.New("provider unavailable")
			}
			if tc.previous {
				sha := strings.Repeat("b", 40)
				if tc.same {
					sha = m.state.Builds[0].SourceSHA
				}
				now := time.Now().UTC()
				m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p", UpdatedAt: now}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", SourceSHA: sha, AlreadyApplied: true}}
			}
			in.AllowRollback, in.Reason = tc.allow, tc.reason
			eventsBefore := len(m.state.Events)
			d, err := s.DeployBuild(context.Background(), in)
			if tc.code != "" {
				if err == nil || !strings.HasPrefix(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "), tc.code) {
					t.Fatal(d, err)
				}
				if len(m.state.Events) != eventsBefore {
					t.Fatal("rejected deployment emitted event")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			if tc.previous && !tc.same {
				calls = 1
			}
			if f.ancestorCalls != calls {
				t.Fatal("provider calls", f.ancestorCalls)
			}
			if tc.check != "" {
				if d.RollbackFromDeploymentID != "current" || d.RollbackFromSHA != strings.Repeat("b", 40) || d.OverrideReason != strings.TrimSpace(tc.reason) {
					t.Fatal(d)
				}
				data := m.state.Events[len(m.state.Events)-1].Data.Data
				if data["allow_rollback"] != true || data["reason"] != d.OverrideReason || data["rollback_from_sha"] != d.RollbackFromSHA || data["rollback_from_deployment_id"] != "current" || data["rollback_check"] != tc.check {
					t.Fatal(data)
				}
			} else if d.RollbackFromSHA != "" || d.RollbackFromDeploymentID != "" || d.OverrideReason != "" {
				t.Fatal(d)
			}
		})
	}
}

func TestDeployBuildInProgressGuard(t *testing.T) {
	for _, tc := range []struct {
		name, profile, repository, path string
		sameIdentity, refused           bool
	}{
		{"same profile", "app", "other/repo", "other.yaml", false, true},
		{"overlapping profile", "other", "o/flux", "clusters/dev/app.yaml", false, true},
		{"separate files", "other", "o/flux", "other.yaml", false, false},
		{"separate repository", "other", "other/repo", "clusters/dev/app.yaml", false, false},
		{"identity dedupe", "app", "o/flux", "clusters/dev/app.yaml", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, _, in := deploymentFixture(t)
			first, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			first.ProfileID, first.GitOpsRepository, first.TargetFiles = tc.profile, tc.repository, []string{tc.path}
			if !tc.sameIdentity {
				first.Identity = "other-identity"
			}
			m.state.Deployments[0] = first
			d, err := s.DeployBuild(context.Background(), in)
			if tc.refused {
				if err == nil || !strings.HasPrefix(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "), "DEPLOYMENT_IN_PROGRESS:") || !strings.Contains(err.Error(), first.ID) || !strings.Contains(err.Error(), first.BuildID) || !strings.Contains(err.Error(), first.Status) {
					t.Fatal(err)
				}
			} else if err != nil || tc.sameIdentity && d.ID != first.ID {
				t.Fatal(d, err)
			}
		})
	}
}

func TestDeployBuildReceiptPrecedesGuards(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	in.RequestKey = "receipt"
	first, err := s.DeployBuild(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	m.state.Deployments[0].Status = "SUCCEEDED"
	m.state.Deployments = append(m.state.Deployments, domain.Deployment{Meta: domain.Meta{ID: "busy", ProductID: "p"}, EnvironmentID: "dev", ProfileID: "app", Identity: "different", Status: "PENDING_CHECKS"})
	m.state.Builds = nil // Replay does not revalidate the build either.
	in.AllowRollback = true
	in.Reason = "" // Changed and invalid override arguments still replay.
	got, err := s.DeployBuild(context.Background(), in)
	if err != nil || got.ID != first.ID || f.ancestorCalls != 0 {
		t.Fatal(got, err)
	}
	in.AllowRollback = false
	in.Reason = "reason without flag"
	got, err = s.DeployBuild(context.Background(), in)
	if err != nil || got.ID != first.ID {
		t.Fatal(got, err)
	}
}

func TestDeployBuildStateChanged(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	now := time.Now().UTC()
	current := domain.Deployment{Meta: domain.Meta{ID: "current", ProductID: "p", UpdatedAt: now}, EnvironmentID: "dev", ProfileID: "app", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}
	m.state.Deployments = []domain.Deployment{current}
	f.ancestorHook = func() {
		if err := s.store.Update(context.Background(), func(st *domain.State) error {
			next := current
			next.ID = "new-current"
			finished := now.Add(time.Second)
			next.FinishedAt = &finished
			st.Deployments = append(st.Deployments, next)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.DeployBuild(context.Background(), in)
	if err == nil || !strings.HasPrefix(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "), "DEPLOYMENT_STATE_CHANGED: retry deploy_build") || len(m.state.Deployments) != 2 {
		t.Fatal(err, m.state.Deployments)
	}
}

func TestDeployBuildInProgressRecheckedAfterAncestry(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p"}, EnvironmentID: "dev", ProfileID: "app", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}}
	f.ancestorHook = func() {
		if err := s.store.Update(context.Background(), func(st *domain.State) error {
			st.Deployments = append(st.Deployments, domain.Deployment{Meta: domain.Meta{ID: "busy", ProductID: "p"}, BuildID: "other", EnvironmentID: "dev", ProfileID: "app", Identity: "other", Status: "PENDING_CHECKS"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.DeployBuild(context.Background(), in)
	if err == nil || !strings.HasPrefix(strings.TrimPrefix(err.Error(), ErrValidation.Error()+": "), "DEPLOYMENT_IN_PROGRESS:") || len(m.state.Deployments) != 2 {
		t.Fatal(err, m.state.Deployments)
	}
}

func TestLatestDevDeployment(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-time.Hour)
	st := domain.State{Deployments: []domain.Deployment{
		{Meta: domain.Meta{ID: "finished", ProductID: "p", UpdatedAt: now.Add(time.Hour)}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", FinishedAt: &old},
		{Meta: domain.Meta{ID: "fallback", ProductID: "p", UpdatedAt: now}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", AlreadyApplied: true},
	}}
	for _, d := range []domain.Deployment{
		{Meta: domain.Meta{ID: "failed", ProductID: "p", UpdatedAt: now.Add(time.Hour)}, ProfileID: "app", EnvironmentID: "dev", Status: "FAILED"},
		{Meta: domain.Meta{ID: "other product", ProductID: "other", UpdatedAt: now.Add(time.Hour)}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED"},
		{Meta: domain.Meta{ID: "other profile", ProductID: "p", UpdatedAt: now.Add(time.Hour)}, ProfileID: "other", EnvironmentID: "dev", Status: "SUCCEEDED"},
		{Meta: domain.Meta{ID: "prod", ProductID: "p", UpdatedAt: now.Add(time.Hour)}, ProfileID: "app", EnvironmentID: "prod", Status: "SUCCEEDED"},
	} {
		st.Deployments = append(st.Deployments, d)
	}
	if got := latestDevDeployment(&st, "p", "app"); got.ID != "fallback" {
		t.Fatal(got)
	}
}

func TestDeployBuildCrossProductInProgressRedactsHandle(t *testing.T) {
	s, m, f, in := deploymentFixture(t)
	m.state.Products = append(m.state.Products, domain.Product{Meta: domain.Meta{ID: "other-product"}})
	other := domain.Deployment{
		Meta:    domain.Meta{ID: "private-deployment-id", ProductID: "other-product"},
		BuildID: "private-build-id", ProfileID: "other-profile", EnvironmentID: "dev",
		GitOpsRepository: "o/flux", TargetFiles: []string{"clusters/dev/app.yaml"},
		Identity: "other-identity", Status: "RECONCILIATION_REQUIRED",
	}
	m.state.Deployments = append(m.state.Deployments, other)
	eventsBefore := len(m.state.Events)
	got, err := s.DeployBuild(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS:") {
		t.Fatalf("expected overlapping deployment refusal, got %+v, %v", got, err)
	}
	for _, secret := range []string{other.ID, other.BuildID, other.Status} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("cross-product refusal reveals %q: %v", secret, err)
		}
	}
	if got.ID != "" || len(m.state.Deployments) != 1 || len(m.state.Events) != eventsBefore || f.ancestorCalls != 0 {
		t.Fatalf("refusal changed state or checked ancestry: %+v", got)
	}
}
