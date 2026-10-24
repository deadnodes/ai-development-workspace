package application

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func TestSpecializedCommandsRequireAuthenticatedAuthority(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := openWorkerTestStore(t, kind)
			s := New(store)
			s.ConfigureWorkers(workerAuthority(t))
			workerSeed(t, s)
			before, err := store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{
				"request_candidate_dev_restoration", "confirm_candidate_dev_restoration", "verify_candidate_dev_restoration", "release_candidate_dev_after_restoration",
				"capture_candidate_dev_baseline", "verify_candidate_dev_runtime", "deploy_candidate_dev",
				"reserve_candidate_dev", "release_candidate_dev", "prepare_release_candidate",
				"cancel_native_delivery", "queue_worker_publication", "reconcile_worker_publication",
				"refresh_registry_inventory", "queue_worker_job", "cancel_worker_job", "reconcile_worker_job",
			} {
				for _, principal := range []auth.Principal{
					{ID: "agent/member", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write", "deploy", "admin"}},
					{ID: "agent/fake-admin", Kind: "agent", Role: "admin", Products: []string{"*"}, Permissions: []string{"read", "write", "deploy", "admin"}},
				} {
					ctx := auth.WithPrincipal(context.Background(), principal)
					_, err := s.Execute(ctx, domain.Command{Action: action, Actor: "human/admin", ProductID: "p", IntegrationID: "i", Data: map[string]any{"main_sources": []any{}}})
					if !errors.Is(err, auth.ErrForbidden) {
						t.Fatalf("%s/%s: %v", action, principal.ID, err)
					}
				}
			}
			after, err := store.Read(context.Background())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("denied commands mutated state: %v", err)
			}
			admin := auth.WithPrincipal(context.Background(), auth.Principal{ID: "human/verified", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
			value, err := s.Execute(admin, queueWorkerCommand(s))
			if err != nil {
				t.Fatal(err)
			}
			if value.(domain.WorkerJob).Actor != "human/verified" {
				t.Fatal("caller spoofed specialized command actor")
			}
		})
	}
}

// Change ownership between the preflight read and the real transaction.
// The command must reauthorize inside Update rather than trusting the read.
type ownershipDriftStore struct{ Store }

func (s ownershipDriftStore) Update(ctx context.Context, change func(*domain.State) error) error {
	return s.Store.Update(ctx, func(st *domain.State) error {
		for i := range st.Features {
			if st.Features[i].ID == "f" {
				st.Features[i].ProductID = "foreign"
			}
		}
		return change(st)
	})
}

func TestCommandAuthorizationRechecksAtomicOwnership(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := openWorkerTestStore(t, kind)
			s := New(store)
			workerSeed(t, s)
			before, err := store.Read(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			s.store = ownershipDriftStore{store}
			_, err = s.Execute(agentContext("write"), domain.Command{Action: "record_progress", Actor: "human/admin", ProductID: "p", FeatureID: "f", Data: map[string]any{"title": "must not persist"}})
			if !errors.Is(err, auth.ErrForbidden) {
				t.Fatal("ownership drift accepted", err)
			}
			after, err := store.Read(context.Background())
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("denied transaction was not rolled back: %v", err)
			}
		})
	}
}
