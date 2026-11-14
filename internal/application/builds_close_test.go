package application

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestCloseBuildActiveAndAdmission(t *testing.T) {
	for _, status := range []string{"DISPATCHED", "RUNNING", "RECONCILIATION_REQUIRED"} {
		t.Run(status, func(t *testing.T) {
			s, m, f, _ := buildFixture(t)
			b := queue(t, s, "first")
			b.Status, b.FailureCode = status, "DISPATCH_UNCERTAIN"
			b.ProviderRunID, b.RunURL, b.RunStatus = 42, "https://git/run/42", "running"
			b.Images = []domain.BuildImage{{ID: "historical", ImageRef: "registry.example/o/r", Digest: "sha256:historical"}}
			m.state.Builds[0] = b
			f.head = strings.Repeat("b", 40)
			next := queue(t, s, "next")
			tick(t, s)
			if getBuild(t, s, next).Status != "QUEUED" {
				t.Fatal("pool did not block")
			}
			b = getBuild(t, s, b)
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "human/operator", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
			in := domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: "  abandoned by operator  ", RequestKey: "close"}
			closed, err := s.CloseBuild(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			expected := b
			expected.Status, expected.FailureCode, expected.FailureMessage = "FAILED", "OPERATOR_CLOSED", "abandoned by operator"
			expected.UpdatedAt, expected.FinishedAt = closed.UpdatedAt, closed.FinishedAt
			if closed.FinishedAt == nil || !reflect.DeepEqual(expected, closed) || buildActive(closed) {
				t.Fatalf("evidence changed: %+v", closed)
			}
			event := m.state.Events[len(m.state.Events)-1]
			if event.Action != "close_build" || event.Actor != "human/operator" || event.Data.Data["previous_status"] != status || event.Data.Data["previous_failure_code"] != b.FailureCode || event.Data.Data["reason"] != closed.FailureMessage || event.Data.Data["provider_run_id"] != int64(42) {
				t.Fatalf("%+v", event)
			}
			count := len(m.state.Events)
			replay, err := s.CloseBuild(ctx, in)
			if err != nil || !reflect.DeepEqual(replay, closed) || count != len(m.state.Events) {
				t.Fatal("replay", err)
			}
			in.Reason = "different reason"
			if _, err = s.CloseBuild(ctx, in); err == nil {
				t.Fatal("key collision")
			}
			stale := b
			stale.Status = "SUCCEEDED"
			if err = s.advanceBuild(context.Background(), b, stale, false); err == nil {
				t.Fatal("stale dispatcher overwrote closure")
			}
			tick(t, s)
			if getBuild(t, s, next).Status != "DISPATCHED" || !reflect.DeepEqual(getBuild(t, s, b), closed) {
				t.Fatal("admission or terminal protection failed")
			}
		})
	}
}

func TestCloseBuildValidation(t *testing.T) {
	for _, status := range []string{"QUEUED", "FAILED", "SUCCEEDED", "CANCELLED"} {
		s, m, _, _ := buildFixture(t)
		b := queue(t, s, "one")
		m.state.Builds[0].Status = status
		_, err := s.CloseBuild(context.Background(), domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: "valid reason"})
		if err == nil || status == "QUEUED" && !strings.Contains(err.Error(), "cancel_build") {
			t.Fatal(status, err)
		}
	}
	for _, reason := range []string{"", "       ", "short", strings.Repeat("x", 501)} {
		s, m, _, _ := buildFixture(t)
		b := queue(t, s, "one")
		m.state.Builds[0].Status = "RUNNING"
		if _, err := s.CloseBuild(context.Background(), domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: reason}); err == nil {
			t.Fatal("invalid reason")
		}
	}
	s, m, _, _ := buildFixture(t)
	b := queue(t, s, "one")
	m.state.Builds[0].Status = "RUNNING"
	writer := auth.WithPrincipal(context.Background(), auth.Principal{ID: "writer", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	if _, err := s.CloseBuild(writer, domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: "valid reason"}); err == nil {
		t.Fatal("non-admin accepted")
	}
}

// Inject a dispatcher write after CloseBuild's read and before its transaction.
type closeRaceStore struct {
	*memoryStore
	before func()
}

func (m *closeRaceStore) Update(ctx context.Context, fn func(*domain.State) error) error {
	if m.before != nil {
		f := m.before
		m.before = nil
		f()
	}
	return m.memoryStore.Update(ctx, fn)
}
func TestCloseBuildConcurrentTransition(t *testing.T) {
	s, m, _, _ := buildFixture(t)
	b := queue(t, s, "one")
	m.state.Builds[0].Status = "RUNNING"
	s.store = &closeRaceStore{memoryStore: m, before: func() { m.state.Builds[0].Status = "SUCCEEDED"; m.state.Builds[0].UpdatedAt = time.Now().UTC() }}
	if _, err := s.CloseBuild(context.Background(), domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: "valid reason"}); err == nil || !strings.Contains(err.Error(), "build changed") {
		t.Fatal(err)
	}
	if m.state.Builds[0].Status != "SUCCEEDED" {
		t.Fatal("lost update")
	}
}

func TestCloseBuildReasonBoundaries(t *testing.T) {
	for _, n := range []int{8, 500} {
		s, m, _, _ := buildFixture(t)
		b := queue(t, s, "one")
		m.state.Builds[0].Status = "RUNNING"
		reason := strings.Repeat("я", n)
		closed, err := s.CloseBuild(context.Background(), domain.CloseBuildRequest{ProductID: "p", BuildID: b.ID, Reason: "  " + reason + "  "})
		if err != nil || closed.FailureMessage != reason {
			t.Fatal(n, err)
		}
	}
}

func TestCloseBuildPostgresReplayAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, reopen := openWorkerTestStore(t, "postgres")
	defer func() { store.Close() }()
	if err := store.Update(ctx, func(st *domain.State) error {
		st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}})
		st.Builds = append(st.Builds, domain.Build{Meta: domain.Meta{ID: "build", ProductID: "p"}, Status: "RUNNING"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	in := domain.CloseBuildRequest{ProductID: "p", BuildID: "build", Reason: "abandoned by operator", RequestKey: "close"}
	closed, err := New(store).CloseBuild(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "FAILED" || len(before.Events) != 1 || before.Events[0].Action != "close_build" {
		t.Fatalf("closure not persisted: %+v, events: %+v", closed, before.Events)
	}
	store.Close()
	store = reopen()
	s := New(store)
	replay, err := s.CloseBuild(ctx, in)
	if err != nil || !reflect.DeepEqual(replay, closed) {
		t.Fatalf("replay: %+v, error: %v; want %+v", replay, err, closed)
	}
	after, err := store.Read(ctx)
	if err != nil || !reflect.DeepEqual(after.Events, before.Events) {
		t.Fatalf("replay changed journal: %+v, error: %v", after.Events, err)
	}
	in.Reason = "different close reason"
	if _, err := s.CloseBuild(ctx, in); err == nil || err.Error() != "validation: REQUEST_KEY_CONFLICT" {
		t.Fatalf("expected REQUEST_KEY_CONFLICT, got %v", err)
	}
	after, err = store.Read(ctx)
	if err != nil || !reflect.DeepEqual(after.Events, before.Events) {
		t.Fatalf("conflict changed journal: %+v, error: %v", after.Events, err)
	}
}
