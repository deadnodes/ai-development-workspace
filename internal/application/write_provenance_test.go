package application

import (
	"context"
	"errors"
	"reflect"
	"releasecontrol/internal/auth"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func projectedWriteState(st domain.State) domain.State {
	st.Events = nil
	return st
}
func writeProjection(st domain.State) domain.State {
	events := st.Events
	st = projectedWriteState(st)
	for _, e := range events {
		if domain.IsWriteProvenanceEvent(e) {
			st.Events = append(st.Events, e)
		}
	}
	return st
}

func TestWriteProjectionSourceRetentionProvenanceParity(t *testing.T) {
	_, m, _, parent := retentionFlowFixture(t)
	for _, restore := range []bool{false, true} {
		st := m.state
		if restore {
			st.Events = append(append([]domain.Event{}, st.Events...), domain.Event{ID: "restore-barrier", Action: "restore_backup", At: time.Now()})
		}
		scoped := writeProjection(st)
		a := sourceRetentionProvenance(&st, *operationByID(&st, parent))
		b := sourceRetentionProvenance(&scoped, *operationByID(&scoped, parent))
		if (a == nil) != (b == nil) || a != nil && a.Error() != b.Error() {
			t.Fatalf("provenance changed by projection: %v / %v", a, b)
		}
	}
}

func TestWriteProjectionPinValidationAndRetentionParity(t *testing.T) {
	s, m, command := artifactPinFixture(t)
	pin := exec(t, s, command).(domain.ArtifactPin)
	for _, release := range []bool{false, true} {
		if release {
			exec(t, s, domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: pin.ID, Data: map[string]any{"application_id": "app"}})
		}
		st := m.state
		scoped := writeProjection(st)
		a, b := domain.ValidateArtifactPins(st), domain.ValidateArtifactPins(scoped)
		if a != nil || b != nil {
			t.Fatalf("valid pin rejected: %v / %v", a, b)
		}
		// The retention planner fingerprints pin audit evidence as well as records.
		// Compare the complete projection rather than only its pin count.
		now := time.Now().UTC()
		full := domain.EvaluateRetention(st, "p", now, nil)
		narrow := domain.EvaluateRetention(scoped, "p", now, nil)
		if !reflect.DeepEqual(full, narrow) {
			t.Fatal("retention plan changed by event projection")
		}
	}
}

func TestWriteProjectionWorkerAdmissionParity(t *testing.T) {
	f, parent := buildActionFixture(t, "local", "CHECK_FAILED", 1)
	seed := buildActionState(t, f)
	for _, kind := range []string{"valid", "missing", "duplicate", "foreign", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			st := seed
			st.Events = append([]domain.Event{}, seed.Events...)
			for i, e := range st.Events {
				if e.Action != "worker_report" {
					continue
				}
				switch kind {
				case "missing":
					st.Events = append(st.Events[:i:i], st.Events[i+1:]...)
				case "duplicate":
					st.Events = append(st.Events, e)
				case "foreign":
					st.Events[i].ProductID = "foreign"
				case "malformed":
					st.Events[i].Data.Data = map[string]any{"fence": "bad"}
				}
				break
			}
			narrow := writeProjection(st)
			for _, job := range st.WorkerJobs {
				a, b := buildFailureReportAdmission(&st, job)
				x, y := buildFailureReportAdmission(&narrow, job)
				if a != x || b != y {
					t.Fatalf("admission changed: %v %v / %v %v", a, b, x, y)
				}
			}
			now := time.Now()
			a := operationBuildFailureAction(&st, *operationByID(&st, parent), now)
			b := operationBuildFailureAction(&narrow, *operationByID(&narrow, parent), now)
			if !reflect.DeepEqual(a, b) {
				t.Fatal("failure context changed by projection")
			}
		})
	}
}

func TestPostgresCommandAuthorizationKeepsUnprojectedEventScope(t *testing.T) {
	store, _ := openWorkerTestStore(t, "postgres")
	defer store.Close()
	_, m := fixture(t)
	m.state.Products = append(m.state.Products, domain.Product{Meta: domain.Meta{ID: "other"}, Name: "Other"})
	m.state.Events = append(m.state.Events, domain.Event{ID: "foreign-event", ProductID: "other", Action: "record_progress"}, domain.Event{ID: "own-event", ProductID: "p", Action: "record_progress"})
	m.state.Operations = append(m.state.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: "chain", ProductID: "p"}, ParentID: "foreign-event", Status: "SUCCEEDED"})
	if err := store.Update(context.Background(), func(st *domain.State) error { *st = m.state; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/member", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	svc := New(store)
	for _, reference := range []string{"foreign-event", "chain"} {
		command := domain.Command{Action: "record_progress", FeatureID: "f", Data: map[string]any{"title": reference}}
		if _, err := svc.Execute(ctx, command); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("preflight admitted %s: %v", reference, err)
		}
		// Exercise the admission inside the revision-locked mutation independently
		// of preflight, so a prior successful check cannot grant stale authority.
		wrapper := commandAuthorizationStore{Store: store, command: command}
		if err := wrapper.Update(ctx, func(st *domain.State) error { t.Fatal("unauthorized callback ran"); return nil }); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("transaction admitted %s: %v", reference, err)
		}
	}
	if _, err := svc.Execute(ctx, domain.Command{Action: "record_progress", ID: "own-event", FeatureID: "f", Data: map[string]any{"title": "duplicate"}}); !errors.Is(err, ErrValidation) {
		t.Fatal("unprojected audit ID reused", err)
	}
	if _, err := svc.Execute(ctx, domain.Command{Action: "record_progress", FeatureID: "f", Data: map[string]any{"title": "permitted"}}); err != nil {
		t.Fatal(err)
	}
}
