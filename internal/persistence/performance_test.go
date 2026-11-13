package persistence_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"regexp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"sort"
	"testing"
	"time"
)

// Run only against a disposable CREATE DATABASE ... TEMPLATE rcp_snapshot clone.
func TestSnapshotWritePerformance(t *testing.T) {
	dsn := os.Getenv("RCP_PERF_DATABASE_URL")
	if dsn == "" {
		t.Skip("RCP_PERF_DATABASE_URL not configured")
	}
	u, err := url.Parse(dsn)
	if err != nil || !regexp.MustCompile(`^/rcp_perf_[0-9]+$`).MatchString(u.Path) {
		t.Fatal("requires disposable rcp_perf_N database (numeric N)")
	}
	ctx := context.Background()
	if os.Getenv("RCP_PERF_AUTHENTICATED") == "1" {
		ctx = auth.WithPrincipal(ctx, auth.Principal{ID: "agent/performance-test", Kind: "agent", Role: "member", Products: []string{"*"}, Permissions: []string{"read", "write"}})
	}
	store, err := persistence.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := application.New(store)
	// Select a real feature using the bounded SQL metadata path, outside timing.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var featureID string
	if err := conn.QueryRow(ctx, "SELECT id FROM records WHERE kind='features' ORDER BY position,id LIMIT 1").Scan(&featureID); err != nil {
		t.Fatal(err)
	}
	durations := make([]time.Duration, 20)
	runID := time.Now().UnixNano()
	for i := range durations {
		c := domain.Command{Action: "record_progress", Actor: "agent/performance-test", FeatureID: featureID, Data: map[string]any{"title": "Disposable snapshot performance measurement"}}
		if os.Getenv("RCP_PERF_MIXED") == "1" {
			switch i % 3 {
			case 1:
				c.Action = "create_integration"
				c.ID = fmt.Sprintf("perf-%d-%d", runID, i)
				c.Data = map[string]any{"title": "Disposable integration", "objective": "Measure an explicit creation identity"}
			case 2:
				c.Action = "update_feature"
				c.Data = map[string]any{"title": "Disposable performance feature"}
			}
		}
		start := time.Now()
		_, err := svc.Execute(ctx, c)
		durations[i] = time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("write %02d (%s): %s", i+1, c.Action, durations[i])
	}
	cold := durations[0]
	warmMax := time.Duration(0)
	for _, d := range durations[1:] {
		if d > warmMax {
			warmMax = d
		}
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("cold=%s p50=%s p95=%s max=%s warm_max=%s (20 writes)", cold, durations[9], durations[18], durations[19], warmMax)
}
