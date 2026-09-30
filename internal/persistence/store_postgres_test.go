package persistence

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"releasecontrol/internal/domain"
)

func TestPostgresHistoricalUpdatePreservesRecordsAndTransactions(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured; PostgreSQL not verified")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_tests") {
		t.Fatal("TEST_DATABASE_URL must be a dedicated *_tests database URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("persistence_test_%d", time.Now().UnixNano())
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, e := pool.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); e != nil {
			t.Error("test schema cleanup:", e)
		}
	}()
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	store, err := Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const operationCount = 20000
	const eventCount = 25000
	err = store.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Migration history"}, {Meta: domain.Meta{ID: "q"}, Name: "Retained"}}
		for i := 0; i < operationCount; i++ {
			st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprintf("op-%08d", i), ProductID: "p", Actor: "historical"}, Kind: "REFRESH_GIT", Status: "SUCCEEDED", Detail: "Retained immutable historical evidence"})
		}
		for i := 0; i < eventCount; i++ {
			st.Events = append(st.Events, domain.Event{ID: fmt.Sprintf("event-%08d", i), ProductID: "p", Actor: "historical", Action: "record_progress"})
		}
		return nil
	})
	if err != nil {
		t.Fatal("seed history:", err)
	}
	started := time.Now()
	err = store.Update(ctx, func(st *domain.State) error {
		st.Operations[0].Detail = "New observation"
		st.Events = append(st.Events, domain.Event{ID: "new-event", ProductID: "p", Actor: "verified-agent", Action: "record_progress"})
		return nil
	})
	if err != nil {
		t.Fatal("historical update:", err)
	}
	t.Logf("20,000 retained operations + 25,000 retained events; one update and event append: %s", time.Since(started))
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Operations) != operationCount || len(st.Events) != eventCount+1 || st.Operations[0].ID != "op-00000000" || st.Operations[0].Detail != "New observation" || st.Events[eventCount].ID != "new-event" {
		t.Fatal("history, identity, order or append lost")
	}
	// Reordering unchanged records does not rewrite their positions in the legacy
	// SQL contract; preserve that behavior while indexing equality checks.
	if err = store.Update(ctx, func(st *domain.State) error { slices.Reverse(st.Operations); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil || st.Operations[0].ID != "op-00000000" {
		t.Fatal("unchanged row position changed", err)
	}
	// Ordinary omissions remain non-deleting while their product still exists.
	if err = store.Update(ctx, func(st *domain.State) error { st.Operations = st.Operations[1:]; return nil }); err != nil {
		t.Fatal(err)
	}
	st, err = store.Read(ctx)
	if err != nil || len(st.Operations) != operationCount {
		t.Fatal("ordinary record omission deleted history", err)
	}
	var revision int64
	if err = store.pool.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("deliberate callback rejection")
	if err = store.Update(ctx, func(st *domain.State) error { st.Operations[0].Detail = "must rollback"; return rejected }); !errors.Is(err, rejected) {
		t.Fatal("callback rejection lost", err)
	}
	if err = store.Update(ctx, func(st *domain.State) error { st.Operations[0].Status = "INVALID"; return nil }); err == nil {
		t.Fatal("invalid status committed")
	}
	var afterRevision int64
	if err = store.pool.QueryRow(ctx, "SELECT revision FROM control_revision WHERE id=1").Scan(&afterRevision); err != nil || afterRevision != revision {
		t.Fatal("failed transaction advanced revision", err)
	}
	st, err = store.Read(ctx)
	if err != nil || st.Operations[0].Detail != "New observation" || st.Operations[0].Status != "SUCCEEDED" {
		t.Fatal("rollback changed historical record", err)
	}
	if err = store.Update(ctx, func(st *domain.State) error { st.Products = st.Products[1:]; st.Operations = nil; return nil }); err != nil {
		t.Fatal("product deletion:", err)
	}
	st, err = store.Read(ctx)
	if err != nil || len(st.Products) != 1 || st.Products[0].ID != "q" || len(st.Operations) != 0 || len(st.Events) != eventCount+1 {
		t.Fatal("product deletion changed retained product/audit policy", err)
	}
}
