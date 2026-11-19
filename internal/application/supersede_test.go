package application

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"releasecontrol/internal/domain"
)

func assertSupersedeEvent(t *testing.T, st domain.State, action, oldID, by, sha, head string) {
	t.Helper()
	count := 0
	for _, e := range st.Events {
		if e.Action == action && e.EntityID == oldID {
			count++
			data := e.Data.Data
			if data["superseded_by"] != by || data["source_sha"] != sha || data["new_source_sha"] != head {
				t.Fatal(data)
			}
		}
	}
	if count != 1 {
		t.Fatalf("got %d %s events", count, action)
	}
}

func TestQueueBuildSuperseded(t *testing.T) {
	for _, tc := range []struct {
		name, status, scope   string
		ancestor, unavailable bool
	}{
		{name: "queued ancestor", status: "QUEUED", ancestor: true},
		{name: "dispatched", status: "DISPATCHED", ancestor: true},
		{name: "running", status: "RUNNING", ancestor: true},
		{name: "reconciliation", status: "RECONCILIATION_REQUIRED", ancestor: true},
		{name: "diverged", status: "QUEUED"},
		{name: "older request", status: "QUEUED"},
		{name: "ancestry error no longer relevant", status: "QUEUED", ancestor: true, unavailable: true},
		{name: "other branch", status: "QUEUED", scope: "branch", ancestor: true},
		{name: "other profile", status: "QUEUED", scope: "profile", ancestor: true},
		{name: "other product", status: "QUEUED", scope: "product", ancestor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, _ := buildFixture(t)
			if tc.name == "older request" {
				f.head = strings.Repeat("b", 40)
			}
			old := queue(t, s, "old")
			m.state.Builds[0].Status = tc.status
			switch tc.scope {
			case "branch":
				m.state.Builds[0].Branch = "feature"
			case "profile":
				m.state.Builds[0].ProfileID = "other"
			case "product":
				m.state.Builds[0].ProductID = "other"
			}
			before := m.state.Builds[0]
			store := &ciLockStore{memoryStore: m}
			s.store = store
			f.head = strings.Repeat("b", 40)
			if tc.name == "older request" {
				f.head = strings.Repeat("a", 40)
			}
			calls := 0
			f.ancestorFunc = func(context.Context, string, string) (bool, error) {
				calls++
				return tc.ancestor, errors.New("ancestry must not gate branch coalescing")
			}
			next := queue(t, s, "new")
			got := m.state.Builds[0]
			superseded := tc.status == "QUEUED" && tc.scope == "" && true
			if superseded {
				if got.Status != "CANCELLED" || got.FailureCode != "SUPERSEDED" || got.SupersededBy != next.ID || got.FinishedAt == nil {
					t.Fatal(got)
				}
				assertSupersedeEvent(t, m.state, "supersede_build", old.ID, next.ID, old.SourceSHA, next.SourceSHA)
				// A stale worker snapshot cannot claim the cancelled pool position.
				claimed := old
				claimed.Status = "DISPATCHED"
				if err := s.advanceBuild(context.Background(), old, claimed, true); !errors.Is(err, errBuildTerminal) {
					t.Fatal(err)
				}
				tick(t, s)
				if f.dispatches != 1 || m.state.Builds[1].Status != "DISPATCHED" || m.state.Builds[0].Status != "CANCELLED" {
					t.Fatal("wrong dispatch", m.state.Builds)
				}
			} else if !reflect.DeepEqual(before, got) {
				t.Fatal("ineligible build changed", got)
			}
			if calls != 0 {
				t.Fatal("branch coalescing unexpectedly checked ancestry", calls)
			}
			count := calls
			replay, err := s.QueueBuild(context.Background(), domain.QueueBuildRequest{ProductID: "p", ProfileID: "app", RequestKey: "old"})
			if err != nil || replay.ID != old.ID || calls != count || len(m.state.Builds) != 2 {
				t.Fatal("replay changed", replay, err)
			}
		})
	}
}

func TestQueueBuildSupersedeRevalidatesClaim(t *testing.T) {
	// Branch builds supersede without ancestry I/O; only QUEUED builds of the same
	// product, profile and branch are eligible.
	for _, change := range []string{"status", "branch", "profile", "product"} {
		t.Run(change, func(t *testing.T) {
			s, m, f, _ := buildFixture(t)
			queue(t, s, "old")
			switch change {
			case "status":
				m.state.Builds[0].Status = "DISPATCHED"
			case "branch":
				m.state.Builds[0].Branch = "other"
			case "profile":
				m.state.Builds[0].ProfileID = "other"
			case "product":
				m.state.Builds[0].ProductID = "other"
			}
			f.head = strings.Repeat("b", 40)
			queue(t, s, "new")
			if m.state.Builds[0].SupersededBy != "" {
				t.Fatal("ineligible build superseded")
			}
		})
	}
}

func TestDeployBuildSuperseded(t *testing.T) {
	for _, tc := range []struct {
		name, status          string
		ancestor, unavailable bool
	}{
		{"pending ancestor", "PENDING_CHECKS", true, false},
		{"diverged", "PENDING_CHECKS", false, false},
		{"older request", "PENDING_CHECKS", false, false},
		{"ancestry error", "PENDING_CHECKS", true, true},
		{"proposing", "PROPOSING", true, false},
		{"merging", "MERGING", true, false},
		{"reconciliation", "RECONCILIATION_REQUIRED", true, false},
		{"rollout", "VERIFYING_ROLLOUT", true, false},
		{"rollout diverged", "VERIFYING_ROLLOUT", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			if tc.name == "older request" {
				m.state.Builds[0].SourceSHA = strings.Repeat("b", 40)
			}
			in.RequestKey = "old"
			old, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			m.state.Deployments[0].Status = tc.status
			if tc.status == "VERIFYING_ROLLOUT" {
				m.state.Deployments[0].RolloutVerified = true
			}
			before := copyDeployment(m.state.Deployments[0])
			b := copyBuild(m.state.Builds[0])
			b.ID = "new-build"
			b.SourceSHA = strings.Repeat("b", 40)
			if tc.name == "older request" {
				b.SourceSHA = strings.Repeat("a", 40)
			}
			b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
			b.Images[0].ImageRef = "registry.example/o/r:main-bbbbbbbb@" + b.Images[0].Digest
			m.state.Builds = append(m.state.Builds, b)
			store := &ciLockStore{memoryStore: m}
			s.store = store
			f.ancestor, f.ancestorErr = tc.ancestor, nil
			if tc.unavailable {
				f.ancestorErr = errors.New("unavailable")
			}
			f.ancestorHook = func() {
				if store.locked {
					t.Fatal("ancestry I/O under store lock")
				}
			}
			nextIn := in
			nextIn.BuildID = b.ID
			nextIn.RequestKey = "new"
			next, err := s.DeployBuild(context.Background(), nextIn)
			if (tc.status == "PENDING_CHECKS" || tc.status == "VERIFYING_ROLLOUT") && tc.ancestor && !tc.unavailable {
				if err != nil || next.Status != "PENDING_CHECKS" {
					t.Fatal(next, err)
				}
				got := m.state.Deployments[0]
				if got.Status != "FAILED" || got.FailureCode != "SUPERSEDED" || got.SupersededBy != next.ID || got.FinishedAt == nil {
					t.Fatal(got)
				}
				if tc.status == "VERIFYING_ROLLOUT" && !got.RolloutVerified {
					t.Fatal("supersede cleared rollout evidence", got)
				}
				assertSupersedeEvent(t, m.state, "supersede_deployment", old.ID, next.ID, old.SourceSHA, next.SourceSHA)
				stale := old
				stale.Status = "PROPOSING"
				if err := s.advanceDeployment(context.Background(), old, &stale, true); err == nil {
					t.Fatal("stale worker claimed superseded deployment")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS") {
					t.Fatal(next, err)
				}
				if !reflect.DeepEqual(before, m.state.Deployments[0]) || len(m.state.Deployments) != 1 {
					t.Fatal("blocked request mutated state")
				}
			}
			calls := f.ancestorCalls
			replay, err := s.DeployBuild(context.Background(), in)
			if err != nil || replay.ID != old.ID || f.ancestorCalls != calls {
				t.Fatal("replay changed", replay, err)
			}
		})
	}
}

func TestDeploySupersedeRevalidatesAndPreservesRollbackGuard(t *testing.T) {
	for _, change := range []string{"status", "sha", "new build sha", "rollback"} {
		t.Run(change, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			old, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			b := copyBuild(m.state.Builds[0])
			b.ID = "next"
			b.SourceSHA = strings.Repeat("b", 40)
			b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
			b.Images[0].ImageRef = "registry.example/o/r:main-bbbbbbbb@" + b.Images[0].Digest
			m.state.Builds = append(m.state.Builds, b)
			f.ancestor = true
			if change == "rollback" {
				current := old
				current.ID = "current"
				current.Status = "SUCCEEDED"
				current.SourceSHA = strings.Repeat("c", 40)
				m.state.Deployments = append(m.state.Deployments, current)
			} else {
				f.ancestorHook = func() {
					switch change {
					case "status":
						m.state.Deployments[0].Status = "MERGING"
					case "sha":
						m.state.Deployments[0].SourceSHA = strings.Repeat("d", 40)
					case "new build sha":
						m.state.Builds[1].SourceSHA = strings.Repeat("e", 40)
					}
				}
			}
			in.BuildID = b.ID
			_, err = s.DeployBuild(context.Background(), in)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if change == "rollback" && !strings.Contains(err.Error(), "ROLLBACK_REFUSED") {
				t.Fatal(err)
			}
			if m.state.Deployments[0].SupersededBy != "" {
				t.Fatal("refused request superseded old deployment")
			}
		})
	}
}

func TestSupersedeAllEligibleHandles(t *testing.T) {
	t.Run("builds", func(t *testing.T) {
		s, m, f, _ := buildFixture(t)
		first := queue(t, s, "first")
		f.head = strings.Repeat("b", 40)
		second := queue(t, s, "second")
		f.head = strings.Repeat("c", 40)
		f.ancestorFunc = func(_ context.Context, old, head string) (bool, error) {
			return (old == first.SourceSHA || old == second.SourceSHA) && head == f.head, nil
		}
		next := queue(t, s, "third")
		// Each newer branch build replaces the queued predecessor immediately.
		for _, c := range []struct{ old, by domain.Build }{{first, second}, {second, next}} {
			got, err := s.GetBuild(context.Background(), domain.BuildLookup{ProductID: "p", BuildID: c.old.ID})
			if err != nil || got.SupersededBy != c.by.ID || got.Status != "CANCELLED" {
				t.Fatal(got, err)
			}
			assertSupersedeEvent(t, m.state, "supersede_build", c.old.ID, c.by.ID, c.old.SourceSHA, c.by.SourceSHA)
		}
		count := len(m.state.Events)
		duplicate := queue(t, s, "new-key-same-sha")
		if duplicate.ID != next.ID || len(m.state.Events) != count {
			t.Fatal("deduplication changed")
		}
	})
	for _, block := range []bool{false, true} {
		name := "deployments"
		if block {
			name = "deployment atomic refusal"
		}
		t.Run(name, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			old, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			second := old
			second.ID = "second"
			second.SourceSHA = strings.Repeat("b", 40)
			second.Identity = "second"
			if block {
				second.Status = "MERGING"
			}
			m.state.Deployments = append(m.state.Deployments, second)
			b := copyBuild(m.state.Builds[0])
			b.ID = "new"
			b.SourceSHA = strings.Repeat("c", 40)
			b.Images[0].Digest = "sha256:" + strings.Repeat("c", 64)
			b.Images[0].ImageRef = "registry.example/o/r:main-cccccccc@" + b.Images[0].Digest
			m.state.Builds = append(m.state.Builds, b)
			in.BuildID = b.ID
			f.ancestor = true
			before := len(m.state.Events)
			next, err := s.DeployBuild(context.Background(), in)
			if block {
				if err == nil || len(m.state.Events) != before || m.state.Deployments[0].Status != "PENDING_CHECKS" {
					t.Fatal("partial supersede on refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []domain.Deployment{old, second} {
				got, err := s.GetDeployment(context.Background(), domain.DeploymentLookup{ProductID: "p", DeploymentID: d.ID})
				if err != nil || got.SupersededBy != next.ID || got.Status != "FAILED" {
					t.Fatal(got, err)
				}
				assertSupersedeEvent(t, m.state, "supersede_deployment", d.ID, next.ID, d.SourceSHA, next.SourceSHA)
			}
		})
	}
}

func TestQueueBuildSameSHADifferentDigestNotSuperseded(t *testing.T) {
	s, m, f, _ := buildFixture(t)
	old := queue(t, s, "old")
	// A changed authority allows a fresh build of the same source instead of
	// deduplicating it. The old artifact does not make its source older.
	m.state.Builds[0].AuthorityHash = "previous-authority"
	digest := "sha256:" + strings.Repeat("b", 64)
	m.state.Builds[0].Images = []domain.BuildImage{{ID: "app", Digest: digest, ImageRef: "registry.example/o/r:main-aaaaaaaa@" + digest}}
	other := copyBuild(m.state.Builds[0])
	other.ID = "same-sha-different-digest"
	other.Images[0].Digest = "sha256:" + strings.Repeat("c", 64)
	other.Images[0].ImageRef = "registry.example/o/r:main-aaaaaaaa@" + other.Images[0].Digest
	m.state.Builds = append(m.state.Builds, other)
	before := []domain.Build{copyBuild(m.state.Builds[0]), copyBuild(m.state.Builds[1])}
	events := len(m.state.Events)
	calls := 0
	f.ancestorFunc = func(context.Context, string, string) (bool, error) {
		calls++
		return true, nil // Git ancestry includes equality.
	}
	next := queue(t, s, "new")
	if next.ID == old.ID || next.SourceSHA != old.SourceSHA || len(m.state.Builds) != 3 {
		t.Fatal("expected a fresh same-SHA build", next)
	}
	if !reflect.DeepEqual(before, m.state.Builds[:2]) || len(m.state.Events) != events+1 || calls != 0 {
		t.Fatal("same-SHA build was checked or changed", m.state.Builds[0], calls)
	}
}

func TestDeployBuildSameSHADifferentDigestNotSuperseded(t *testing.T) {
	for _, differentDigest := range []bool{false, true} {
		name := "same digest deduplicates"
		if differentDigest {
			name = "different digest blocks"
		}
		t.Run(name, func(t *testing.T) {
			s, m, f, in := deploymentFixture(t)
			in.RequestKey = "old"
			old, err := s.DeployBuild(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			before := copyDeployment(m.state.Deployments[0])
			b := copyBuild(m.state.Builds[0])
			b.ID = "same-sha-build"
			if differentDigest {
				b.Images[0].Digest = "sha256:" + strings.Repeat("b", 64)
				b.Images[0].ImageRef = "registry.example/o/r:main-aaaaaaaa@" + b.Images[0].Digest
			}
			m.state.Builds = append(m.state.Builds, b)
			events := len(m.state.Events)
			f.ancestor = true // Git ancestry includes equality.
			nextIn := in
			nextIn.BuildID, nextIn.RequestKey = b.ID, "new"
			next, err := s.DeployBuild(context.Background(), nextIn)
			if differentDigest {
				if err == nil || !strings.Contains(err.Error(), "DEPLOYMENT_IN_PROGRESS") {
					t.Fatal(next, err)
				}
			} else if err != nil || next.ID != old.ID {
				t.Fatal("deduplication changed", next, err)
			}
			if !reflect.DeepEqual(before, m.state.Deployments[0]) || len(m.state.Deployments) != 1 || len(m.state.Events) != events || f.ancestorCalls != 0 {
				t.Fatal("same-SHA deployment was checked or changed", m.state.Deployments, f.ancestorCalls)
			}
			replay, err := s.DeployBuild(context.Background(), in)
			if err != nil || replay.ID != old.ID {
				t.Fatal("replay changed", replay, err)
			}
		})
	}
}
