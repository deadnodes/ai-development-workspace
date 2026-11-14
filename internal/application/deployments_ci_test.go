package application

import (
	"context"
	"errors"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/providers/forgejo"
	"strings"
	"testing"
	"time"
)

type treeGateFake struct {
	*deploymentFake
	pr                        forgejo.PR
	found, equal              bool
	prErr, treeErr            error
	headStatus                forgejo.CommitStatus
	hook                      func()
	calls, prCalls, treeCalls int
}

func (f *treeGateFake) CommitStatus(ctx context.Context, _ buildconfig.Connection, _, sha string) (forgejo.CommitStatus, error) {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	if sha == f.pr.HeadSHA {
		return f.headStatus, nil
	}
	return f.status, f.statusErr
}
func (f *treeGateFake) CommitPullRequest(context.Context, buildconfig.Connection, string, string) (forgejo.PR, bool, error) {
	f.prCalls++
	if f.hook != nil {
		f.hook()
	}
	return f.pr, f.found, f.prErr
}
func (f *treeGateFake) TreesEqual(context.Context, buildconfig.Connection, string, string, string) (bool, error) {
	f.treeCalls++
	if f.hook != nil {
		f.hook()
	}
	return f.equal, f.treeErr
}
func treeGateFixture(t *testing.T) (*Service, *memoryStore, *treeGateFake, domain.DeployBuildRequest) {
	s, m, base, in := deploymentFixture(t)
	f := &treeGateFake{deploymentFake: base, found: true, equal: true, pr: forgejo.PR{Number: 7, Merged: true, BaseRef: s.builds.config.Profiles[0].DefaultBranch, HeadSHA: strings.Repeat("e", 40)}, headStatus: forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "success"}}}}
	f.status = forgejo.CommitStatus{State: "pending"}
	if err := s.ConfigureDeployments(f, f, f); err != nil {
		t.Fatal(err)
	}
	return s, m, f, in
}
func TestPRTreeGate(t *testing.T) {
	cases := []struct {
		name             string
		change           func(*Service, *treeGateFake)
		status, evidence string
	}{
		{"main cancelled", func(_ *Service, f *treeGateFake) {
			f.status = cancelledCIStatus()
		}, "SUCCEEDED", "pr_tree"},
		{"main cancelled without PR", func(_ *Service, f *treeGateFake) {
			f.status = cancelledCIStatus()
			f.found = false
		}, "PENDING_CHECKS", ""},
		{"head cancelled", func(_ *Service, f *treeGateFake) {
			f.headStatus = cancelledCIStatus()
		}, "PENDING_CHECKS", ""},
		{"main green", func(_ *Service, f *treeGateFake) { f.status = f.headStatus }, "SUCCEEDED", "main"},
		{"equal", func(*Service, *treeGateFake) {}, "SUCCEEDED", "pr_tree"},
		{"different", func(_ *Service, f *treeGateFake) { f.equal = false }, "PENDING_CHECKS", ""},
		{"main failure", func(_ *Service, f *treeGateFake) {
			f.status = forgejo.CommitStatus{State: "failure", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "failure"}}}
		}, "FAILED", ""},
		{"unmerged", func(_ *Service, f *treeGateFake) { f.pr.Merged = false }, "PENDING_CHECKS", ""},
		{"wrong base", func(_ *Service, f *treeGateFake) { f.pr.BaseRef = "other" }, "PENDING_CHECKS", ""},
		{"not found", func(_ *Service, f *treeGateFake) { f.found = false }, "PENDING_CHECKS", ""},
		{"PR unavailable", func(_ *Service, f *treeGateFake) { f.prErr = errors.New("unavailable") }, "PENDING_CHECKS", ""},
		{"tree unavailable", func(_ *Service, f *treeGateFake) { f.treeErr = errors.New("unavailable") }, "PENDING_CHECKS", ""},
		{"head pending", func(_ *Service, f *treeGateFake) { f.headStatus.State = "pending"; f.headStatus.Statuses = nil }, "PENDING_CHECKS", ""},
		{"head failed", func(_ *Service, f *treeGateFake) { f.headStatus.Statuses[0].Status = "failure" }, "PENDING_CHECKS", ""},
		{"disabled", func(s *Service, _ *treeGateFake) {
			s.builds.config.Profiles[0].CIGate = &buildconfig.CIGate{AcceptPRTree: false}
		}, "PENDING_CHECKS", ""},
		{"context mapping", func(s *Service, f *treeGateFake) {
			s.builds.config.Profiles[0].RequiredStatusContexts = []string{"check (push)", "exact (push) inner", "plain"}
			f.headStatus.Statuses = []forgejo.StatusContext{{Context: "check (pull_request)", Status: "success"}, {Context: "exact (push) inner", Status: "success"}, {Context: "plain", Status: "success"}}
		}, "SUCCEEDED", "pr_tree"},
		{"exact suffix only", func(s *Service, f *treeGateFake) {
			s.builds.config.Profiles[0].RequiredStatusContexts = []string{"verify"}
			f.headStatus.Statuses[0].Context = "verify (pull_request)"
		}, "PENDING_CHECKS", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, in := treeGateFixture(t)
			tc.change(s, f)
			if _, err := s.DeployBuild(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			deployTick(t, s)
			d := m.state.Deployments[0]
			if d.Status != tc.status || d.CIEvidence != tc.evidence {
				t.Fatalf("got %+v", d)
			}
			if d.CIEvidence == "pr_tree" && (d.CIPRNumber != 7 || d.CIPRHeadSHA != f.pr.HeadSHA || d.MainCIState != "pending") {
				t.Fatalf("missing evidence: %+v", d)
			}
			if (tc.name == "main failure" || tc.name == "main green" || tc.name == "disabled") && f.prCalls != 0 {
				t.Fatal("unexpected PR lookup")
			}
		})
	}
}

func TestMainCISafetyNet(t *testing.T) {
	for _, state := range []string{"pending", "success", "failure", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			s, m, f, in := treeGateFixture(t)
			if _, err := s.DeployBuild(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			deployTick(t, s)
			old := time.Now().Add(-2 * time.Minute)
			m.state.Deployments[0].MainCICheckedAt = &old
			f.status = forgejo.CommitStatus{State: state, Statuses: []forgejo.StatusContext{{Context: "verify", Status: state}, {Context: "unrelated", Status: "failure"}}}
			if state == "cancelled" {
				f.status = cancelledCIStatus()
			}
			before := f.calls
			deployTick(t, s)
			d := m.state.Deployments[0]
			if d.MainCIState != state || d.MainCICheckedAt == nil || f.calls != before+1 || d.Status != "SUCCEEDED" {
				t.Fatalf("got %+v calls %d", d, f.calls)
			}
			failures := 0
			for _, e := range m.state.Events {
				if e.Action == "main_ci_failed" {
					failures++
					raw := e.Data.Data["failing_contexts"].(map[string]string)
					if len(raw) != 1 || raw["verify"] != "failure" {
						t.Fatal(raw)
					}
				}
			}
			attention, err := s.Query(context.Background(), "list_attention", "p")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range attention.([]map[string]any) {
				if item["reason"] == "MAIN_CI_FAILED" {
					found = true
					if item["deployment_id"] != d.ID || item["profile_id"] != d.ProfileID {
						t.Fatal(item)
					}
				}
			}
			if (failures == 1) != (state == "failure") || found != (state == "failure") {
				t.Fatalf("events %d attention %v", failures, attention)
			}
			deployTick(t, s)
			if f.calls != before+1 {
				t.Fatal("polled twice within minute")
			}
			m.state.Deployments[0].MainCICheckedAt = &old
			deployTick(t, s)
			if state != "pending" && f.calls != before+1 {
				t.Fatal("terminal CI polled again")
			}
		})
	}
}
func TestMainCISafetyNetExpiry(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	old := time.Now().Add(-3*time.Hour - time.Second)
	m.state.Deployments[0].MergedAt = &old
	m.state.Deployments[0].MainCICheckedAt = &old
	calls := f.calls
	deployTick(t, s)
	if f.calls != calls || m.state.Deployments[0].MainCIState != "pending" {
		t.Fatal("expired poll")
	}
}

// Detect provider calls inside either borrowed views or update callbacks.
type ciLockStore struct {
	*memoryStore
	locked bool
}

func (m *ciLockStore) View(_ context.Context, fn func(*domain.State) error) error {
	m.locked = true
	defer func() { m.locked = false }()
	return fn(&m.state)
}
func (m *ciLockStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	m.locked = true
	defer func() { m.locked = false }()
	return m.memoryStore.Update(ctx, fn)
}
func TestPRTreeGateIOOutsideStoreLock(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	store := &ciLockStore{memoryStore: m}
	s.store = store
	f.hook = func() {
		if store.locked {
			t.Fatal("provider I/O under store lock")
		}
	}
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	old := time.Now().Add(-2 * time.Minute)
	m.state.Deployments[0].MainCICheckedAt = &old
	deployTick(t, s)
}
func TestPRTreeGateDoesNotReuseStaleEvidence(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	f.registryErr = errors.New("unavailable")
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	if m.state.Deployments[0].CIEvidence != "pr_tree" {
		t.Fatal("expected initial evidence")
	}
	f.found = false
	f.registryErr = nil
	deployTick(t, s)
	if m.state.Deployments[0].Status != "PENDING_CHECKS" || f.proposals != 0 {
		t.Fatal("stale evidence authorized deployment")
	}
}

func TestMainCISafetyNetUnavailableAndRollout(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	old := time.Now().Add(-2 * time.Minute)
	// Observe independently from rollout and retain a bounded retry timestamp on I/O error.
	m.state.Deployments[0].MainCICheckedAt = &old
	m.state.Deployments[0].Status = "VERIFYING_ROLLOUT"
	deadline := time.Now().Add(time.Hour)
	m.state.Deployments[0].RolloutDeadline = &deadline
	f.statusErr = errors.New("unavailable")
	calls := f.calls
	deployTick(t, s)
	d := m.state.Deployments[0]
	if d.MainCIState != "pending" || d.Status != "VERIFYING_ROLLOUT" || !d.MainCICheckedAt.After(old) || f.calls != calls+1 {
		t.Fatalf("got %+v", d)
	}
	deployTick(t, s)
	if f.calls != calls+1 {
		t.Fatal("retried unavailable CI within minute")
	}
}

func TestMainCISafetyNetRejectsStaleObservation(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	old := time.Now().Add(-2 * time.Minute)
	m.state.Deployments[0].MainCICheckedAt = &old
	f.status = forgejo.CommitStatus{State: "failure", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "failure"}}}
	f.hook = func() { m.state.Deployments[0].CloseReason = "concurrent change" }
	deployTick(t, s)
	if m.state.Deployments[0].MainCIState != "pending" || len(mainCIAttention(m.state, "p")) != 0 {
		t.Fatal("stale result persisted")
	}
	for _, e := range m.state.Events {
		if e.Action == "main_ci_failed" {
			t.Fatal("stale event")
		}
	}
}

func TestTrustedProducerBothGates(t *testing.T) {
	for _, gate := range []string{"main", "pr"} {
		for _, producer := range []string{"actions", "user", "other repo"} {
			t.Run(gate+"/"+producer, func(t *testing.T) {
				s, m, f, in := treeGateFixture(t)
				s.builds.config.Profiles[0].CIGate = nil
				p := s.builds.config.Profiles[0]
				status := forgejo.StatusContext{Context: "verify", Status: "success", TargetURL: "/" + strings.ToUpper(p.Repository) + "/actions/runs/47/jobs/0"}
				if producer == "user" {
					status.Creator = &forgejo.StatusCreator{Login: "attacker"}
				}
				if producer == "other repo" {
					status.TargetURL = "/other/repository/actions/runs/47/jobs/0"
				}
				v := forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{status}}
				if gate == "main" {
					f.status = v
					f.found = false
				} else {
					f.headStatus = v
				}
				if _, err := s.DeployBuild(context.Background(), in); err != nil {
					t.Fatal(err)
				}
				deployTick(t, s)
				if (m.state.Deployments[0].Status == "SUCCEEDED") != (producer == "actions") {
					t.Fatalf("%+v", m.state.Deployments[0])
				}
			})
		}
	}
}
func TestNewestTrustedStatus(t *testing.T) {
	p := buildconfig.Profile{Repository: "o/r"}
	statuses := []forgejo.StatusContext{
		{ID: 3, Context: "check", Status: "success", Creator: &forgejo.StatusCreator{Login: "user"}, TargetURL: "/o/r/actions/runs/1/jobs/0"},
		{ID: 1, Context: "check", Status: "success", TargetURL: "/o/r/actions/runs/1/jobs/0"},
		{ID: 2, Context: "check", Status: "failure", TargetURL: "/o/r/actions/runs/2/jobs/0"},
	}
	green, failed, _ := ciVerdict(trustedCIStatus(forgejo.CommitStatus{Statuses: statuses}, p), []string{"check"})
	if green || !failed {
		t.Fatal("did not select newest trusted status")
	}
	for _, url := range []string{"https://example/o/r/actions/runs/1/jobs/0", "/o/r/actions/runs/1/jobs/0?x=1", "/o/r/actions/runs/x/jobs/0", "/o/r/actions/runs/1/jobs/0/"} {
		statuses[0].Creator = nil
		statuses[0].TargetURL = url
		green, _, _ := ciVerdict(trustedCIStatus(forgejo.CommitStatus{Statuses: statuses[:1]}, p), []string{"check"})
		if green {
			t.Fatal(url)
		}
	}
}

type hangingCISource struct {
	*deploymentFake
	calls chan string
}

func (f *hangingCISource) CommitStatus(ctx context.Context, _ buildconfig.Connection, _, sha string) (forgejo.CommitStatus, error) {
	f.calls <- sha
	if sha != strings.Repeat("e", 40) {
		<-ctx.Done()
		return forgejo.CommitStatus{}, ctx.Err()
	}
	return forgejo.CommitStatus{State: "success", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "success"}}}, nil
}
func TestMainCISafetyNetHangingFirstAcrossPasses(t *testing.T) {
	s, m, f, _ := treeGateFixture(t)
	source := &hangingCISource{deploymentFake: f.deploymentFake, calls: make(chan string, 20)}
	s.deployments.source = source
	old := time.Now().Add(-2 * time.Minute)
	merged := time.Now().Add(-time.Minute)
	for _, sha := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)} {
		m.state.Deployments = append(m.state.Deployments, domain.Deployment{Meta: domain.Meta{ID: sha, ProductID: "p"}, ProfileID: s.builds.config.Profiles[0].ID, AuthorityHash: s.builds.deploymentHashes[s.builds.config.Profiles[0].ID], SourceSHA: sha, CIEvidence: "pr_tree", MergedAt: &merged, MainCIState: "pending", MainCICheckedAt: &old})
	}
	for pass := 0; pass < 2; pass++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		if err := s.watchMainCI(ctx); err != nil {
			t.Fatal(err)
		}
		cancel()
		if pass == 1 && m.state.Deployments[4].MainCIState != "success" {
			t.Fatal("hanging first deployment blocked later result")
		}
	}
	// The fifth item starts in pass two even while the first remains pending.
	seen := map[string]bool{}
	for len(source.calls) > 0 {
		seen[<-source.calls] = true
	}
	for _, d := range m.state.Deployments {
		if !seen[d.SourceSHA] {
			t.Fatalf("starved %s", d.SourceSHA)
		}
	}
}

func cancelledCIStatus() forgejo.CommitStatus {
	return forgejo.CommitStatus{State: "failure", Statuses: []forgejo.StatusContext{{Context: "verify", Status: "failure", Description: "Has been cancelled", TargetURL: "/o/r/actions/runs/1/jobs/0"}}}
}

func TestCancelledCIVerdict(t *testing.T) {
	for _, required := range [][]string{nil, {"verify"}} {
		for _, producer := range []string{"forgejo_actions", "any"} {
			p := buildconfig.Profile{Repository: "o/r", CIGate: &buildconfig.CIGate{TrustedProducers: producer}}
			v := cancelledCIStatus()
			green, failed, contexts := ciVerdict(trustedCIStatus(v, p), required)
			if green || failed || contexts["verify"] != "cancelled" {
				t.Fatalf("%s %v: %v %v %v", producer, required, green, failed, contexts)
			}
			for _, description := range []string{"Has failed", "Has been cancelled ", "has been cancelled", ""} {
				v.Statuses[0].Description = description
				green, failed, _ = ciVerdict(trustedCIStatus(v, p), required)
				if green || !failed {
					t.Fatalf("misclassified %q", description)
				}
			}
			v = cancelledCIStatus()
			v.Statuses = append(v.Statuses, forgejo.StatusContext{Context: "other", Status: "failure", TargetURL: "/o/r/actions/runs/1/jobs/1"})
			_, failed, _ = ciVerdict(trustedCIStatus(v, p), nil)
			if !failed {
				t.Fatal("cancellation masked real failure")
			}
		}
	}
}

func TestCancelledMainCIWaitsUntilDeadline(t *testing.T) {
	s, m, f, in := treeGateFixture(t)
	f.status = cancelledCIStatus()
	f.found = false
	if _, err := s.DeployBuild(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	deployTick(t, s)
	if m.state.Deployments[0].Status != "PENDING_CHECKS" {
		t.Fatal(m.state.Deployments[0])
	}
	m.state.Deployments[0].Deadline = time.Now().Add(-time.Second)
	deployTick(t, s)
	if d := m.state.Deployments[0]; d.Status != "FAILED" || d.FailureCode != "CI_NOT_GREEN" {
		t.Fatal(d)
	}
}
