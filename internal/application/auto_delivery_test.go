package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/domain"
)

func autoBool(v bool) *bool { return &v }

type autoForgejo struct {
	*fakeForgejo
	at      time.Time
	timeErr error
	hook    func()
}

func (f *autoForgejo) CommitTime(context.Context, buildconfig.Connection, string, string) (time.Time, error) {
	if f.hook != nil {
		f.hook()
	}
	return f.at, f.timeErr
}
func (f *autoForgejo) BranchHead(ctx context.Context, c buildconfig.Connection, r, b string) (string, error) {
	if f.hook != nil {
		f.hook()
	}
	return f.fakeForgejo.BranchHead(ctx, c, r, b)
}
func autoBuildFixture(t *testing.T, at time.Time) (*Service, *memoryStore, *autoForgejo) {
	s, m, base, r := buildFixture(t)
	f := &autoForgejo{fakeForgejo: base, at: at}
	c := buildConfig()
	c.Profiles[0].AutoBuild = autoBool(true)
	if err := s.ConfigureBuilds(c, f, r); err != nil {
		t.Fatal(err)
	}
	return s, m, f
}
func TestAutoBuildHeadChangeAndRestart(t *testing.T) {
	now := time.Now().UTC()
	s, m, f := autoBuildFixture(t, now.Add(-time.Hour))
	s.pollAutoBuilds(context.Background(), now)
	if len(m.state.Builds) != 0 {
		t.Fatal("startup burst")
	}
	f.head = strings.Repeat("b", 40)
	s.pollAutoBuilds(context.Background(), now.Add(19*time.Second))
	if len(m.state.Builds) != 0 {
		t.Fatal("poll interval ignored")
	}
	s.pollAutoBuilds(context.Background(), now.Add(21*time.Second))
	s.pollAutoBuilds(context.Background(), now.Add(42*time.Second))
	if len(m.state.Builds) != 1 || f.branchCalls != 3 {
		t.Fatalf("builds %d calls %d", len(m.state.Builds), f.branchCalls)
	}
	b := m.state.Builds[0]
	if b.RequestKey != "auto-build:app:"+f.head || b.Actor != "system/auto-build" || b.Trigger != "auto" || b.Branch != "main" {
		t.Fatalf("%+v", b)
	}
	if m.state.Events[len(m.state.Events)-1].Actor != "system/auto-build" {
		t.Fatal("actor")
	}
	s.builds.setAutoHead("app", nil)
	s.pollAutoBuilds(context.Background(), now.Add(time.Minute))
	if len(m.state.Builds) != 1 {
		t.Fatal("restart duplicated")
	}
}
func TestAutoBuildRecentRecoveryAndRetry(t *testing.T) {
	now := time.Now().UTC()
	s, m, f := autoBuildFixture(t, now.Add(-time.Minute))
	f.timeErr = errors.New("temporary")
	s.pollAutoBuilds(context.Background(), now)
	if len(m.state.Builds) != 0 {
		t.Fatal("timestamp failed open")
	}
	f.timeErr = nil
	s.pollAutoBuilds(context.Background(), now.Add(21*time.Second))
	s.pollAutoBuilds(context.Background(), now.Add(42*time.Second))
	if len(m.state.Builds) != 1 {
		t.Fatal("recent head not recovered once")
	}
}
func TestAutoDeliveryDisabledAndNonAuto(t *testing.T) {
	now := time.Now()
	s, m, f := autoBuildFixture(t, now)
	t.Setenv("RCP_AUTO_DELIVERY", "off")
	s.pollAutoBuilds(context.Background(), now)
	if f.branchCalls != 0 || len(m.state.Builds) != 0 {
		t.Fatal("kill switch")
	}
	t.Setenv("RCP_AUTO_DELIVERY", "on")
	s.builds.config.Profiles[0].AutoBuild = nil
	s.pollAutoBuilds(context.Background(), now)
	if f.branchCalls != 0 {
		t.Fatal("non-auto polled")
	}
}
func autoDeployFixture(t *testing.T) (*Service, *memoryStore) {
	s, m, _, _ := deploymentFixture(t)
	s.builds.config.Profiles[0].AutoDeployDev = autoBool(true)
	m.state.Builds[0].Branch = "main"
	m.state.Builds[0].QueuedAt = time.Now().UTC()
	s.builds.setAutoHead("app", &autoHead{seen: m.state.Builds[0].SourceSHA, observed: time.Now()})
	return s, m
}
func TestAutoDeployNewestAndReplay(t *testing.T) {
	s, m := autoDeployFixture(t)
	older := m.state.Builds[0]
	older.ID = "older"
	older.QueuedAt = older.QueuedAt.Add(-time.Minute)
	queued := older
	queued.ID = "queued"
	queued.Status = "QUEUED"
	queued.QueuedAt = time.Now().Add(time.Minute)
	m.state.Builds = append(m.state.Builds, older, queued)
	s.autoDeployBuilds(context.Background())
	s.builds.autoDone = nil // restart: persisted handle/receipt must prevent replay
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 1 || len(m.state.DeploymentReceipts) != 1 {
		t.Fatal("duplicate/missing deployment")
	}
	d := m.state.Deployments[0]
	if d.BuildID != "build" || d.RequestKey != "auto-deploy:build" || d.Actor != "system/auto-deploy" || d.Trigger != "auto" {
		t.Fatalf("%+v", d)
	}
	// Explicit request-key replay uses the existing handle.
	replay, err := s.DeployBuild(context.Background(), domain.DeployBuildRequest{ProductID: "p", BuildID: "build", EnvironmentID: "dev", RequestKey: d.RequestKey})
	if err != nil || replay.ID != d.ID {
		t.Fatal(replay, err)
	}
}
func TestAutoDeployBranchAndKillSwitch(t *testing.T) {
	s, m := autoDeployFixture(t)
	m.state.Builds[0].Branch = "feature"
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 0 {
		t.Fatal("feature deployed")
	}
	m.state.Builds[0].Branch = "main"
	t.Setenv("RCP_AUTO_DELIVERY", "off")
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 0 {
		t.Fatal("kill switch")
	}
	t.Setenv("RCP_AUTO_DELIVERY", "on")
	s.builds.config.Profiles[0].AutoDeployDev = nil
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 0 {
		t.Fatal("non-auto deployed")
	}
}
func TestAutoDeployRollbackNotRetriedAndInProgressRetries(t *testing.T) {
	for _, rollback := range []bool{true, false} {
		t.Run(map[bool]string{true: "rollback", false: "in-progress"}[rollback], func(t *testing.T) {
			s, m := autoDeployFixture(t)
			now := time.Now().UTC()
			status := "MERGING"
			if rollback {
				status = "SUCCEEDED"
			}
			m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "existing", ProductID: "p", CreatedAt: now, UpdatedAt: now}, ProfileID: "app", EnvironmentID: "dev", BuildID: "other", SourceSHA: strings.Repeat("b", 40), Status: status, Identity: "different", AuthorityHash: s.builds.deploymentHashes["app"]}}
			calls := 0
			source := s.deployments.source.(*deploymentFake)
			source.ancestor = true
			source.ancestorHook = func() { calls++ }
			s.autoDeployBuilds(context.Background())
			if len(m.state.Deployments) != 1 {
				t.Fatal("guard ignored")
			}
			if rollback {
				s.autoDeployBuilds(context.Background())
				if calls != 1 {
					t.Fatalf("rollback retried %d", calls)
				}
			} else {
				m.state.Deployments = nil
				s.autoDeployBuilds(context.Background())
				if len(m.state.Deployments) != 1 || m.state.Deployments[0].BuildID != "build" {
					t.Fatal("in-progress not retried")
				}
			}
		})
	}
}

type autoLockStore struct {
	*memoryStore
	locked bool
}

func (m *autoLockStore) View(_ context.Context, fn func(*domain.State) error) error {
	m.locked = true
	defer func() { m.locked = false }()
	return fn(&m.state)
}
func (m *autoLockStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	m.locked = true
	defer func() { m.locked = false }()
	return m.memoryStore.Update(ctx, fn)
}
func TestAutoDeliveryIOOutsideStoreLock(t *testing.T) {
	now := time.Now().UTC()
	s, m, f := autoBuildFixture(t, now)
	store := &autoLockStore{memoryStore: m}
	s.store = store
	f.hook = func() {
		if store.locked {
			t.Error("provider I/O under store lock")
		}
	}
	s.pollAutoBuilds(context.Background(), now)
	f.head = strings.Repeat("c", 40)
	s.pollAutoBuilds(context.Background(), now.Add(21*time.Second))
	if len(m.state.Builds) != 2 {
		t.Fatal("build I/O test did not queue")
	}

	s, m = autoDeployFixture(t)
	store = &autoLockStore{memoryStore: m}
	s.store = store
	m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p"}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}}
	source := s.deployments.source.(*deploymentFake)
	source.ancestorHook = func() {
		if store.locked {
			t.Error("ancestry I/O under store lock")
		}
	}
	s.autoDeployBuilds(context.Background())
	if source.ancestorCalls != 1 || len(m.state.Deployments) != 2 {
		t.Fatal("deployment I/O not exercised")
	}
}

func TestAutoFlagsLegacyHashCompatibility(t *testing.T) {
	s, _, _, _ := deploymentFixture(t)
	a := s.builds
	p := a.config.Profiles[0]
	// Reconstruct the exact pre-flag Profile type, preserving original JSON field order.
	typ := reflect.TypeOf(p)
	fields := []reflect.StructField{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Name != "AutoBuild" && f.Name != "AutoDeployDev" {
			fields = append(fields, f)
		}
	}
	old := reflect.New(reflect.StructOf(fields)).Elem()
	original := reflect.ValueOf(p)
	for _, f := range fields {
		old.FieldByName(f.Name).Set(original.FieldByName(f.Name))
	}
	old.FieldByName("CIGate").SetZero()
	old.FieldByName("Deployments").SetZero()
	old.FieldByName("RequiredStatusContexts").SetZero()
	pool := buildconfig.Pool{ID: p.PoolID, MaxInFlight: a.config.Capacity(p.PoolID)}
	registry := a.config.Registry(p.RegistryID)
	oldBuild := buildFingerprint(struct {
		Profile                            any
		Connection                         buildconfig.Connection
		Pool                               buildconfig.Pool
		Registry                           buildconfig.Registry
		TimeoutSeconds, RunLookbackSeconds int
	}{old.Interface(), a.config.Connection(p.ConnectionID), pool, registry, a.config.TimeoutSeconds, a.config.RunLookbackSeconds})
	gitops, _ := a.config.DevGitOps()
	oldDeploy := buildFingerprint(struct {
		CIGate                 *buildconfig.CIGate        `json:",omitempty"`
		Rollout                *buildconfig.RolloutConfig `json:",omitempty"`
		BuildAuthority         string
		RequiredStatusContexts []string
		Dev                    buildconfig.DeploymentTarget
		GitOps                 buildconfig.GitOps
		Registry               buildconfig.Registry
	}{p.CIGate, rolloutAuthority([]buildconfig.Profile{p}, a.config.Rollout), oldBuild, p.RequiredStatusContexts, p.Deployments["dev"], gitops, registry})
	if oldBuild != a.buildHashes[p.ID] || oldDeploy != a.deploymentHashes[p.ID] {
		t.Fatal("flags absent changed existing hashes")
	}
}

type concurrentAutoForgejo struct {
	*fakeForgejo
	active atomic.Int32
	peak   atomic.Int32
}

func (f *concurrentAutoForgejo) BranchHead(context.Context, buildconfig.Connection, string, string) (string, error) {
	n := f.active.Add(1)
	for {
		peak := f.peak.Load()
		if n <= peak || f.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	f.active.Add(-1)
	return strings.Repeat("a", 40), nil
}
func (f *concurrentAutoForgejo) CommitTime(context.Context, buildconfig.Connection, string, string) (time.Time, error) {
	return time.Now().Add(-time.Hour), nil
}
func TestAutoBuildBoundedConcurrency(t *testing.T) {
	s, _, _, r := buildFixture(t)
	c := buildConfig()
	p := c.Profiles[0]
	c.Profiles = nil
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		copy := p
		copy.ID = name
		copy.AutoBuild = autoBool(true)
		c.Profiles = append(c.Profiles, copy)
	}
	f := &concurrentAutoForgejo{fakeForgejo: &fakeForgejo{}}
	if err := s.ConfigureBuilds(c, f, r); err != nil {
		t.Fatal(err)
	}
	s.pollAutoBuilds(context.Background(), time.Now())
	if f.peak.Load() > 4 || f.peak.Load() < 2 {
		t.Fatalf("concurrency %d", f.peak.Load())
	}
}

func TestAutoDeployRechecksNewestAfterIO(t *testing.T) {
	s, m := autoDeployFixture(t)
	m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p"}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}}
	source := s.deployments.source.(*deploymentFake)
	source.ancestorHook = func() {
		newer := m.state.Builds[0]
		newer.ID = "newer"
		// Same-SHA rebuild must supersede the older handle.
		newer.QueuedAt = newer.QueuedAt.Add(time.Minute)
		m.state.Builds = append(m.state.Builds, newer)
		source.ancestorHook = nil
	}
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 1 {
		t.Fatal("stale successful build deployed after concurrent success")
	}
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 2 || m.state.Deployments[1].BuildID != "newer" {
		t.Fatal("newest build not deployed on next tick")
	}
}

type autoFailStore struct {
	*memoryStore
	fail bool
}

func (m *autoFailStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	if m.fail {
		m.fail = false
		return errors.New("temporary store failure")
	}
	return m.memoryStore.Update(ctx, fn)
}
func TestAutoBuildQueueErrorRetried(t *testing.T) {
	now := time.Now().UTC()
	s, m, f := autoBuildFixture(t, now.Add(-time.Hour))
	store := &autoFailStore{memoryStore: m}
	s.store = store
	s.pollAutoBuilds(context.Background(), now)
	f.head = strings.Repeat("b", 40)
	store.fail = true
	s.pollAutoBuilds(context.Background(), now.Add(21*time.Second))
	if len(m.state.Builds) != 0 {
		t.Fatal("failed queue persisted")
	}
	s.pollAutoBuilds(context.Background(), now.Add(42*time.Second))
	s.pollAutoBuilds(context.Background(), now.Add(63*time.Second))
	if len(m.state.Builds) != 1 || m.state.Builds[0].RequestKey != "auto-build:app:"+f.head {
		t.Fatal("head not retried exactly once")
	}
}

func TestAutoDeployAfterBuildLoopSuccess(t *testing.T) {
	s, m := autoDeployFixture(t)
	m.state.Builds = nil
	sha := strings.Repeat("a", 40)
	s.builds.provider = &fakeForgejo{head: sha}
	s.builds.registry = &fakeBuildRegistry{artifact: buildregistry.Artifact{Digest: "sha256:" + strings.Repeat("d", 64), Revision: sha}}
	b := queue(t, s, "loop")
	tick(t, s)
	if got := getBuild(t, s, b); got.Status != "SUCCEEDED" {
		t.Fatalf("build not succeeded: %+v", got)
	}
	if len(m.state.Deployments) != 1 || m.state.Deployments[0].BuildID != b.ID {
		t.Fatal("same tick did not deploy completed build")
	}
	tick(t, s)
	if len(m.state.Deployments) != 1 {
		t.Fatal("subsequent tick duplicated deployment")
	}
}

func TestAutoDeployRequiresFreshCurrentHead(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "different", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			s, m := autoDeployFixture(t)
			h := s.builds.autoHeadSnapshot("app")
			switch mode {
			case "missing":
				s.builds.setAutoHead("app", nil)
			case "stale":
				h.observed = time.Now().Add(-2*time.Minute - time.Second)
			case "different":
				h.seen = strings.Repeat("c", 40)
			}
			if mode != "missing" {
				s.builds.setAutoHead("app", h)
			}
			s.autoDeployBuilds(context.Background())
			want := 0
			if mode == "fresh" {
				want = 1
			}
			if len(m.state.Deployments) != want {
				t.Fatalf("deployments = %d, want %d", len(m.state.Deployments), want)
			}
		})
	}
}

func TestAutoBuildSameSHAOtherBranch(t *testing.T) {
	s, m, f := autoBuildFixture(t, time.Now())
	b, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", Branch: "feature", RequestKey: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	s.pollAutoBuilds(context.Background(), time.Now())
	if len(m.state.Builds) != 2 || m.state.Builds[1].Branch != "main" || m.state.Builds[1].SourceSHA != f.head {
		t.Fatalf("%+v", m.state.Builds)
	}
	replay, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", Branch: "another", RequestKey: "explicit"})
	if err != nil || replay.ID != b.ID {
		t.Fatalf("explicit dedupe changed: %+v %v", replay, err)
	}
}

func TestAutoDeployOnlyPollsHead(t *testing.T) {
	s, m, f := autoBuildFixture(t, time.Now())
	s.builds.config.Profiles[0].AutoBuild = nil
	s.builds.config.Profiles[0].AutoDeployDev = autoBool(true)
	s.pollAutoBuilds(context.Background(), time.Now())
	if f.branchCalls != 1 || len(m.state.Builds) != 0 || s.builds.autoHeadSnapshot("app").observed.IsZero() {
		t.Fatal("deploy-only observation missing or queued build")
	}
}

func TestAutoDeployRechecksHeadFreshnessAfterIO(t *testing.T) {
	s, m := autoDeployFixture(t)
	m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p"}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}}
	source := s.deployments.source.(*deploymentFake)
	source.ancestorHook = func() {
		h := s.builds.autoHeadSnapshot("app")
		h.observed = time.Now().Add(-3 * time.Minute)
		s.builds.setAutoHead("app", h)
	}
	s.autoDeployBuilds(context.Background())
	if len(m.state.Deployments) != 1 {
		t.Fatal("expired observation deployed after I/O")
	}
}

func TestAutoBuildFailedPollDoesNotRefreshHead(t *testing.T) {
	now := time.Now()
	s, _, f := autoBuildFixture(t, now.Add(-time.Hour))
	s.pollAutoBuilds(context.Background(), now)
	observed := s.builds.autoHeadSnapshot("app").observed
	f.branchError = errors.New("offline")
	s.pollAutoBuilds(context.Background(), now.Add(3*time.Minute))
	if !s.builds.autoHeadSnapshot("app").observed.Equal(observed) {
		t.Fatal("failed poll refreshed head")
	}
}

func TestAutoDeployNewerBuildOfDifferentSHADoesNotSupersede(t *testing.T) {
	for _, afterIO := range []bool{false, true} {
		t.Run(map[bool]string{false: "preflight", true: "after-io"}[afterIO], func(t *testing.T) {
			s, m := autoDeployFixture(t)
			newer := m.state.Builds[0]
			newer.ID = "rebuilt-old-commit"
			newer.SourceSHA = strings.Repeat("c", 40)
			newer.QueuedAt = newer.QueuedAt.Add(time.Minute)
			if afterIO {
				m.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "current", ProductID: "p"}, ProfileID: "app", EnvironmentID: "dev", Status: "SUCCEEDED", SourceSHA: strings.Repeat("b", 40)}}
				source := s.deployments.source.(*deploymentFake)
				source.ancestorHook = func() { m.state.Builds = append(m.state.Builds, newer); source.ancestorHook = nil }
			} else {
				m.state.Builds = append(m.state.Builds, newer)
			}
			s.autoDeployBuilds(context.Background())
			if len(m.state.Deployments) == 0 || m.state.Deployments[len(m.state.Deployments)-1].BuildID != "build" {
				t.Fatal("unrelated SHA blocked current head")
			}
		})
	}
}

func TestAutoDeployConcurrentPollerHeadSnapshots(t *testing.T) {
	s, _ := autoDeployFixture(t)
	s.builds.config.Profiles[0].AutoBuild = nil // Poller writes observations without touching the test store.
	s.builds.provider = &fakeForgejo{head: strings.Repeat("a", 40)}
	done := make(chan struct{})
	start := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for i := 0; i < 500; i++ {
			h := s.builds.autoHeadSnapshot("app")
			h.polled = time.Time{}
			s.builds.setAutoHead("app", h)
			s.builds.mu.Lock()
			s.pollAutoBuilds(context.Background(), time.Now())
			s.builds.mu.Unlock()
		}
	}()
	close(start)
	ctx := context.WithValue(autoContext(context.Background(), "system/auto-deploy", "p"), autoDeployKey{}, true)
	for i := 0; i < 500; i++ {
		// No request-key replay: each request exercises both preflight and Update.
		if _, err := s.DeployBuild(ctx, domain.DeployBuildRequest{ProductID: "p", BuildID: "build", EnvironmentID: "dev"}); err != nil {
			t.Error(err)
			break
		}
	}
	<-done
	snapshot := s.builds.autoHeadSnapshot("app")
	snapshot.seen = "detached"
	if s.builds.autoHeadSnapshot("app").seen == snapshot.seen {
		t.Fatal("snapshot aliases stored observation")
	}
}
