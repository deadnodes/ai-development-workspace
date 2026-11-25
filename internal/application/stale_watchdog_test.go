package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
)

func TestConfigureBuildsPreservesExplicitStaleDisable(t *testing.T) {
	raw, err := json.Marshal(buildConfig())
	if err != nil {
		t.Fatal(err)
	}
	configJSON := strings.TrimSuffix(string(raw), "}") + `,"stale_operation_seconds":0}`
	config, err := buildconfig.Decode(strings.NewReader(configJSON))
	if err != nil {
		t.Fatal(err)
	}
	s := New(&memoryStore{state: domain.EmptyState()})
	if err = s.ConfigureBuilds(config, &fakeForgejo{}, &fakeBuildRegistry{}); err != nil {
		t.Fatal(err)
	}
	if got := s.builds.config.StaleTimeoutSeconds(); got != 0 {
		t.Fatalf("explicit zero became %d", got)
	}
}

func TestBuildWatchdogClosesStaleQueuedAndRunning(t *testing.T) {
	for _, tc := range []struct{ status, want string }{{"QUEUED", "CANCELLED"}, {"RUNNING", "FAILED"}, {"RECONCILIATION_REQUIRED", "RECONCILIATION_REQUIRED"}} {
		t.Run(tc.status+tc.want, func(t *testing.T) {
			s, m, _, _ := buildFixture(t)
			b := queue(t, s, "stale")
			if tc.status != "QUEUED" || tc.want == "FAILED" {
				b.Status = tc.status
				if tc.status == "RUNNING" {
					b.ProviderRunID = 1
				}
			}
			age := 2*time.Hour + time.Minute
			if tc.want == "RECONCILIATION_REQUIRED" {
				age = 2*time.Hour - time.Minute
			}
			b.UpdatedAt, b.StatusChangedAt = time.Now().Add(-age), time.Now().Add(-age)
			m.state.Builds[0] = b
			if err := s.DispatchBuildsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := m.state.Builds[0]
			if got.Status != tc.want {
				t.Fatalf("got %s, want %s", got.Status, tc.want)
			}
			if got.Status != tc.want {
				t.Fatalf("got %s, want %s", got.Status, tc.want)
			}
			if tc.want == "RECONCILIATION_REQUIRED" {
				return
			}
			if got.FailureCode != "STALE_TIMEOUT" || len(m.state.Events) == 0 {
				t.Fatalf("missing stale evidence: %+v", got)
			}
			ev := m.state.Events[len(m.state.Events)-1]
			if ev.Action != "stale_close_build" || ev.Actor != "system/stale-watchdog" || ev.Data.Data["previous_status"] != tc.status || ev.Data.Data["resolution"] != tc.want {
				t.Fatalf("incorrect stale audit: %+v", ev)
			}
		})
	}
}

func TestStatusTransitionResetsStaleTimer(t *testing.T) {
	s, _, _, in := deploymentFixture(t)
	d, err := s.DeployBuild(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	next := d
	next.Status = "RECONCILIATION_REQUIRED"
	if err = s.advanceDeployment(context.Background(), d, &next, false); err != nil {
		t.Fatal(err)
	}
	if next.StatusChangedAt.IsZero() || next.StatusChangedAt.Before(d.StatusChangedAt) {
		t.Fatalf("deployment timer not reset: %v", next.StatusChangedAt)
	}

	bs, bm, _, _ := buildFixture(t)
	b := queue(t, bs, "timer")
	bn := b
	bn.Status = "RUNNING"
	if err = bs.advanceBuild(context.Background(), b, bn, false); err != nil {
		t.Fatal(err)
	}
	if bm.state.Builds[0].StatusChangedAt.IsZero() || bm.state.Builds[0].StatusChangedAt.Before(b.StatusChangedAt) {
		t.Fatalf("build timer not reset: %v", bm.state.Builds[0].StatusChangedAt)
	}
}
