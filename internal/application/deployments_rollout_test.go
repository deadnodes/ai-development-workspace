package application

import (
	"context"
	"errors"
	"fmt"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRollout struct {
	result RolloutResult
	err    error
	hook   func()
}

func (f *fakeRollout) VerifyRollout(_ context.Context, p RolloutPlan) (RolloutResult, error) {
	if f.hook != nil {
		f.hook()
	}
	return f.result, f.err
}
func TestDeploymentRolloutLifecycle(t *testing.T) {
	for _, already := range []bool{false, true} {
		for _, phase := range []string{"READY", "FAILED", "PENDING", "ERROR", "TIMEOUT"} {
			t.Run(phase+map[bool]string{true: "_already", false: "_merge"}[already], func(t *testing.T) {
				s, m, f, in := deploymentFixture(t)
				guarded := &rolloutGuardStore{memoryStore: m}
				s.store = guarded
				f.already = already
				c := *s.builds.config
				c.Profiles[0].Deployments["dev"].Files[0].Rollout = &buildconfig.RolloutTarget{Namespace: "lp-dev", HelmReleases: []string{"pp-back"}}
				oldBuildHash := s.builds.buildHashes["app"]
				oldDeploymentHash := s.builds.deploymentHashes["app"]
				if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
					t.Fatal(err)
				}
				if oldBuildHash != s.builds.buildHashes["app"] || oldDeploymentHash == s.builds.deploymentHashes["app"] {
					t.Fatal("rollout must change only deployment authority")
				}
				runtime := &fakeRollout{result: RolloutResult{Phase: phase, Reason: "UpgradeFailed: pre-upgrade hooks failed: BackoffLimitExceeded"}}
				if phase == "ERROR" {
					runtime.err = errors.New("unavailable")
				}
				s.deployments.runtime = runtime
				if _, err := s.DeployBuild(context.Background(), in); err != nil {
					t.Fatal(err)
				}
				deployTick(t, s)
				d := m.state.Deployments[0]
				if d.Status != "VERIFYING_ROLLOUT" || d.MergedAt == nil || d.RolloutDeadline == nil {
					t.Fatal(d)
				}
				// Verify that provider I/O happens outside both borrowed views and updates.
				runtime.hook = func() {
					if guarded.borrowed {
						t.Fatal("runtime I/O under store lock")
					}
					if err := m.Update(context.Background(), func(*domain.State) error { return nil }); err != nil {
						t.Fatal(err)
					}
				}
				other := m.state.Builds[0]
				other.ID = "other"
				other.Images = append([]domain.BuildImage(nil), other.Images...)
				other.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
				other.Images[0].ImageRef = "registry.example/o/r:main-aaaaaaaa@" + other.Images[0].Digest
				m.state.Builds = append(m.state.Builds, other)
				blocked := in
				blocked.BuildID = "other"
				if _, err := s.DeployBuild(context.Background(), blocked); err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS") {
					t.Fatal(err)
				}
				if phase == "TIMEOUT" {
					past := time.Now().Add(-time.Second)
					m.state.Deployments[0].RolloutDeadline = &past
				}
				deployTick(t, s)
				d = m.state.Deployments[0]
				switch phase {
				case "READY":
					if d.Status != "SUCCEEDED" || !d.RolloutVerified {
						t.Fatal(d)
					}
				case "FAILED":
					if d.FailureCode != "ROLLOUT_FAILED" || !strings.Contains(d.RolloutFailure, "BackoffLimitExceeded") {
						t.Fatal(d)
					}
				case "TIMEOUT":
					if d.FailureCode != "ROLLOUT_TIMEOUT" {
						t.Fatal(d)
					}
				default:
					if d.Status != "VERIFYING_ROLLOUT" {
						t.Fatal(d)
					}
					past := time.Now().Add(-time.Second)
					m.state.Deployments[0].RolloutDeadline = &past
					deployTick(t, s)
					if m.state.Deployments[0].FailureCode != "ROLLOUT_TIMEOUT" {
						t.Fatal(m.state.Deployments)
					}
				}
			})
		}
	}
}
func TestCloseVerifyingRollout(t *testing.T) {
	s, m, _, _ := deploymentFixture(t)
	m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "d", ProductID: "p"}, Status: "VERIFYING_ROLLOUT", PRNumber: 123}}
	out, err := s.CloseDeployment(context.Background(), domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: "d", Reason: "operator abandons rollout"})
	if err != nil || out.FailureCode != "OPERATOR_CLOSED" {
		t.Fatal(out, err)
	}
}
func TestUnverifiedDeploymentSuccess(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	if m.state.Deployments[0].Status != "SUCCEEDED" || m.state.Deployments[0].RolloutVerified {
		t.Fatal(m.state.Deployments)
	}
}

func TestRolloutTimeoutAuthority(t *testing.T) {
	s, _, _, _ := deploymentFixture(t)
	c := *s.builds.config
	buildHash, deployHash := s.builds.buildHashes["app"], s.builds.deploymentHashes["app"]
	c.Profiles[0].Deployments["dev"].Files[0].Rollout = &buildconfig.RolloutTarget{Namespace: "dev", Deployments: []string{"app"}}
	if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	buildHash, deployHash = s.builds.buildHashes["app"], s.builds.deploymentHashes["app"]
	c.Rollout.TimeoutSeconds = 1200
	c.Rollout.InCluster = true
	if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	if buildHash != s.builds.buildHashes["app"] || deployHash == s.builds.deploymentHashes["app"] {
		t.Fatal("top-level rollout must affect deployment authority only")
	}
}

// Borrow-aware store makes the I/O assertion deterministic without a deadlock timeout.
type rolloutGuardStore struct {
	*memoryStore
	borrowed bool
}

func (s *rolloutGuardStore) View(_ context.Context, fn func(*domain.State) error) error {
	s.borrowed = true
	defer func() { s.borrowed = false }()
	return fn(&s.state)
}
func (s *rolloutGuardStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	s.borrowed = true
	defer func() { s.borrowed = false }()
	return s.memoryStore.Update(ctx, fn)
}

func TestRolloutTargetsWithoutVerifier(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	c := *s.builds.config
	c.Profiles[0].Deployments["dev"].Files[0].Rollout = &buildconfig.RolloutTarget{Namespace: "lp-dev", Deployments: []string{"pp-back"}}
	if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	d := m.state.Deployments[0]
	if d.Status != "SUCCEEDED" || d.RolloutVerified || d.RolloutDeadline != nil {
		t.Fatal(d)
	}
}

func TestRolloutPlanReferences(t *testing.T) {
	for _, tc := range []struct{ format, template, want string }{
		{"helm_values", "release-{tag}@{digest}", "release-main-123@sha256:abc"},
		{"container_image", "", "main-123@sha256:abc"},
	} {
		p := buildconfig.Profile{Deployments: map[string]buildconfig.DeploymentTarget{"dev": {Files: []buildconfig.DeploymentFile{{ImageID: "runtime", ImageRepository: "registry:5000/app", Format: tc.format, TagFormat: tc.template, Rollout: &buildconfig.RolloutTarget{Namespace: "dev", Deployments: []string{"app"}}}}}}}
		d := domain.Deployment{Images: []domain.BuildImage{{ID: "runtime", ImageRef: "registry:5000/app:main-123@sha256:abc", Digest: "sha256:abc"}}}
		plan := rolloutPlan(d, p)
		if len(plan.Targets) != 1 || plan.Targets[0].Reference != tc.want || plan.Targets[0].Repository != "registry:5000/app" || plan.Targets[0].Digest != "sha256:abc" {
			t.Fatal(plan)
		}
	}
}

func TestRolloutCloseDuringObservation(t *testing.T) {
	s, m, _, _ := deploymentFixture(t)
	deadline := time.Now().Add(time.Minute)
	d := domain.Deployment{Meta: domain.Meta{ID: "d", ProductID: "p"}, Status: "VERIFYING_ROLLOUT", RolloutDeadline: &deadline}
	m.state.Deployments = []domain.Deployment{d}
	s.deployments.runtime = &fakeRollout{result: RolloutResult{Phase: "READY"}, hook: func() {
		if _, err := s.CloseDeployment(context.Background(), domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: "d", Reason: "operator closes during observation"}); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.verifyDeploymentRollout(context.Background(), d, buildconfig.Profile{}); err == nil {
		t.Fatal("stale observation overwrote closure")
	}
	if got := m.state.Deployments[0]; got.FailureCode != "OPERATOR_CLOSED" || got.RolloutVerified {
		t.Fatal(got)
	}
}

func TestRolloutDeadlineDuringObservation(t *testing.T) {
	s, m, _, _ := deploymentFixture(t)
	deadline := time.Now().Add(time.Minute)
	d := domain.Deployment{Meta: domain.Meta{ID: "d", ProductID: "p"}, Status: "VERIFYING_ROLLOUT", RolloutDeadline: &deadline}
	m.state.Deployments = []domain.Deployment{d}
	s.deployments.runtime = &fakeRollout{result: RolloutResult{Phase: "READY"}, hook: func() { deadline = time.Now().Add(-time.Second) }}
	if err := s.verifyDeploymentRollout(context.Background(), d, buildconfig.Profile{}); err != nil {
		t.Fatal(err)
	}
	if got := m.state.Deployments[0]; got.FailureCode != "ROLLOUT_TIMEOUT" || got.RolloutVerified {
		t.Fatal(got)
	}
}

func TestRolloutWorkloadListAuthority(t *testing.T) {
	s, _, _, _ := deploymentFixture(t)
	c := *s.builds.config
	r := &buildconfig.RolloutTarget{Namespace: "lp-dev", HelmReleases: []string{"api"}}
	c.Profiles[0].Deployments["dev"].Files[0].Rollout = r
	if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	buildHash, deployHash := s.builds.buildHashes["app"], s.builds.deploymentHashes["app"]
	r.HelmReleases = append(r.HelmReleases, "consumer")
	if err := s.ConfigureBuilds(&c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	if buildHash != s.builds.buildHashes["app"] || deployHash == s.builds.deploymentHashes["app"] {
		t.Fatal("workload lists must change only deployment authority")
	}
}

func TestRolloutPlanWorkloadLists(t *testing.T) {
	p := buildconfig.Profile{Deployments: map[string]buildconfig.DeploymentTarget{"dev": {Files: []buildconfig.DeploymentFile{
		{ImageID: "runtime", ImageRepository: "registry/app", TagFormat: "{digest}", Rollout: &buildconfig.RolloutTarget{Namespace: "lp-dev", HelmReleases: []string{"api", "consumer"}}},
		{ImageID: "runtime", ImageRepository: "registry/app", TagFormat: "{digest}", Rollout: &buildconfig.RolloutTarget{Namespace: "meteroid-dev", Deployments: []string{"meteroid-api", "worker"}}},
	}}}}
	plan := rolloutPlan(domain.Deployment{Images: []domain.BuildImage{{ID: "runtime", Digest: "sha256:abc"}}}, p)
	if len(plan.Targets) != 4 {
		t.Fatal(plan)
	}
	for i, target := range plan.Targets {
		if target.Digest != "sha256:abc" || target.Repository != "registry/app" || target.Reference != "sha256:abc" {
			t.Fatal(target)
		}
		if i < 2 {
			if target.Namespace != "lp-dev" || target.HelmRelease != []string{"api", "consumer"}[i] || target.Deployment != "" {
				t.Fatal(target)
			}
		} else {
			if target.Namespace != "meteroid-dev" || target.Deployment != []string{"meteroid-api", "worker"}[i-2] || target.HelmRelease != "" {
				t.Fatal(target)
			}
		}
	}
}

type hangingRollout struct{ t *testing.T }

func (h hangingRollout) VerifyRollout(ctx context.Context, _ RolloutPlan) (RolloutResult, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > rolloutTickTimeout {
		h.t.Error("missing per-tick deadline")
	}
	<-ctx.Done()
	return RolloutResult{}, ctx.Err()
}
func TestRolloutHangingRuntimeBound(t *testing.T) {
	s, m, _, _ := deploymentFixture(t)
	deadline := time.Now().Add(time.Hour)
	d := domain.Deployment{Meta: domain.Meta{ID: "d", ProductID: "p"}, Status: "VERIFYING_ROLLOUT", RolloutDeadline: &deadline}
	m.state.Deployments = []domain.Deployment{d}
	s.deployments.runtime = hangingRollout{t}
	start := time.Now()
	if err := s.verifyDeploymentRollout(context.Background(), d, buildconfig.Profile{}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > rolloutTickTimeout+2*time.Second {
		t.Fatal(elapsed)
	}
	if m.state.Deployments[0].Status != "VERIFYING_ROLLOUT" {
		t.Fatal(m.state.Deployments)
	}
}

func TestDeploymentHashWithoutRolloutMatchesOriginMain(t *testing.T) {
	s, _, _, _ := deploymentFixture(t)
	c := s.builds.config
	p := c.Profiles[0]
	g, _ := c.DevGitOps()
	// Exact deployment fingerprint input from origin/main before rollout support.
	old := buildFingerprint(struct {
		BuildAuthority         string
		RequiredStatusContexts []string
		Dev                    buildconfig.DeploymentTarget
		GitOps                 buildconfig.GitOps
		Registry               buildconfig.Registry
	}{s.builds.buildHashes[p.ID], p.RequiredStatusContexts, p.Deployments["dev"], g, c.Registry(p.RegistryID)})
	if old != s.builds.deploymentHashes[p.ID] {
		t.Fatal("pre-rollout profile hash changed")
	}
	// Exact Config field order and JSON tags from origin/main.
	legacy := buildFingerprint(struct {
		GitOps             []buildconfig.GitOps      `json:"gitops,omitempty"`
		Environments       []buildconfig.Environment `json:"environments,omitempty"`
		Connections        []buildconfig.Connection  `json:"connections"`
		Registries         []buildconfig.Registry    `json:"registries"`
		Pools              []buildconfig.Pool        `json:"pools"`
		Profiles           []buildconfig.Profile     `json:"profiles"`
		TimeoutSeconds     int                       `json:"timeout_seconds,omitempty"`
		RunLookbackSeconds int                       `json:"run_lookback_seconds,omitempty"`
	}{c.GitOps, c.Environments, c.Connections, c.Registries, c.Pools, c.Profiles, c.TimeoutSeconds, c.RunLookbackSeconds})
	if legacy != s.builds.legacyGlobal || !s.builds.deploymentAuthorityMatches(p.ID, legacy) {
		t.Fatal("legacy global hash changed")
	}
	c.Rollout.TimeoutSeconds = 1200
	if err := s.ConfigureBuilds(c, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	if old != s.builds.deploymentHashes[p.ID] {
		t.Fatal("unused rollout settings changed profile hash")
	}
}

type passBudgetRollout struct {
	t            *testing.T
	active, peak atomic.Int32
	before       func()
}

func (h *passBudgetRollout) VerifyRollout(ctx context.Context, _ RolloutPlan) (RolloutResult, error) {
	h.before()
	n := h.active.Add(1)
	defer h.active.Add(-1)
	for old := h.peak.Load(); n > old && !h.peak.CompareAndSwap(old, n); old = h.peak.Load() {
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 25*time.Second {
		h.t.Error("missing dispatcher pass budget")
	}
	<-ctx.Done()
	return RolloutResult{}, ctx.Err()
}
func TestRolloutDispatcherSharedBudgetAndBuildProgress(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	f := &fakeForgejo{head: strings.Repeat("b", 40)}
	if err := s.ConfigureBuilds(s.builds.config, f, &fakeBuildRegistry{err: buildregistry.ErrMissing}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	other := m.state.Deployments[0]
	deadline := time.Now().Add(time.Hour)
	for i := range 12 {
		d := other
		d.ID = fmt.Sprintf("rollout-%02d", i)
		d.TargetFiles = []string{fmt.Sprintf("clusters/dev/other-%d.yaml", i)}
		d.Status = "VERIFYING_ROLLOUT"
		d.RolloutDeadline = &deadline
		m.state.Deployments = append(m.state.Deployments, d)
	}
	// Put pending GitOps work last to check prioritization independently of input order.
	m.state.Deployments = append(m.state.Deployments[1:], other)
	h := &passBudgetRollout{t: t, before: func() {
		// All non-verifying work must finish before any observation starts.
		if m.state.Deployments[len(m.state.Deployments)-1].Status != "SUCCEEDED" {
			t.Error("GitOps work starved")
		}
	}}
	s.deployments.runtime = h
	b := queue(t, s, "after-rollouts")
	start := time.Now()
	if err := s.DispatchDeploymentsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 24*time.Second || elapsed > 28*time.Second {
		t.Fatalf("dispatcher elapsed %s", elapsed)
	}
	if h.peak.Load() != 4 {
		t.Fatalf("peak concurrency = %d", h.peak.Load())
	}
	for _, d := range m.state.Deployments[:12] {
		if d.Status != "VERIFYING_ROLLOUT" || d.RolloutFailure != "Kubernetes API unavailable" {
			t.Fatal(d)
		}
	}
	tick(t, s)
	if got := getBuild(t, s, b); got.Status != "DISPATCHED" || f.dispatches != 1 {
		t.Fatal(got, f.dispatches)
	}
}

func TestRolloutDispatcherAppliesEveryResultWithCAS(t *testing.T) {
	s, m, _, in := deploymentFixture(t)
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	d := m.state.Deployments[0]
	deadline := time.Now().Add(time.Hour)
	d.Status, d.RolloutDeadline = "VERIFYING_ROLLOUT", &deadline
	d.ID = "a-closed"
	other := d
	other.ID = "b-ready"
	m.state.Deployments = []domain.Deployment{d, other}
	var once sync.Once
	s.deployments.runtime = &fakeRollout{result: RolloutResult{Phase: "READY"}, hook: func() {
		once.Do(func() {
			if _, err := s.CloseDeployment(context.Background(), domain.CloseDeploymentRequest{ProductID: "p", DeploymentID: d.ID, Reason: "operator abandons rollout"}); err != nil {
				t.Error(err)
			}
		})
	}}
	if err := s.DispatchDeploymentsOnce(context.Background()); err == nil {
		t.Fatal("expected stale result CAS rejection")
	}
	if got := m.state.Deployments[0]; got.FailureCode != "OPERATOR_CLOSED" || got.RolloutVerified {
		t.Fatal(got)
	}
	if got := m.state.Deployments[1]; got.Status != "SUCCEEDED" || !got.RolloutVerified {
		t.Fatal(got)
	}
}
