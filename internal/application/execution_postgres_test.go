package application

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
)

func TestPostgresConcurrentExecutionDeliveryClaimHasOneWinner(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_tests") {
		t.Fatal("TEST_DATABASE_URL must be a dedicated *_tests database URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	schema := fmt.Sprintf("execution_claim_test_%d", time.Now().UnixNano())
	if _, err = pool.Exec(ctx, "CREATE SCHEMA "+pgxIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DROP SCHEMA "+pgxIdentifier(schema)+" CASCADE")
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	store, err := persistence.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Update(ctx, func(st *domain.State) error {
		now := time.Now().UTC()
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}}
		st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p"}, Status: "planned"}}
		st.ExecutionEvents = []domain.ExecutionEvent{{ID: "ev", ProductID: "p", IntegrationID: "i", CreatedAt: now, Delivery: domain.ExecutionEventDelivery{NextAttemptAt: now.Add(-time.Second)}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(store)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, e := svc.Execute(ctx, domain.Command{Action: "claim_execution_event_delivery", Actor: "test", ProductID: "p", Data: map[string]any{"event_id": "ev", "supervisor_id": "sup"}})
			if e == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent delivery claim winners=%d, want 1", wins)
	}
}

func pgxIdentifier(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\"\"") + "\"" }
