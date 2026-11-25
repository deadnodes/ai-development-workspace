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
	"releasecontrol/internal/auth"
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

func TestPostgresClaimExecutionUsesHumanOwner(t *testing.T) {
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
	schema := fmt.Sprintf("execution_owner_test_%d", time.Now().UnixNano())
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
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "P"}}
		st.Features = []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "F", Goal: "G", Status: "active"}}
		st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i", ProductID: "p", FeatureID: "f"}, Status: "planned"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(store)
	claim := func(owner string) error {
		ctx := auth.WithPrincipal(ctx, auth.Principal{ID: "agent/" + owner, Owner: "human/" + owner, Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
		_, e := svc.Execute(ctx, domain.Command{Action: "claim_execution", ProductID: "p", IntegrationID: "i", Data: map[string]any{"orchestrator_id": "o", "agent_id": "a"}})
		return e
	}
	if err = claim("A"); err != nil {
		t.Fatal(err)
	}
	if err = claim("B"); err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
		t.Fatalf("expected NOT_ASSIGNEE, got %v", err)
	}
	st, err := store.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Integrations[0].Assignee != "human/A" {
		t.Fatalf("assignee=%q", st.Integrations[0].Assignee)
	}
}

func TestPostgresExecutionIsolation(t *testing.T) {
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
	schema := fmt.Sprintf("execution_isolation_test_%d", time.Now().UnixNano())
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
	now := time.Now().UTC()
	if err = store.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "P"}}
		st.Features = []domain.Feature{{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "F", Goal: "G", Status: "active"}}
		st.Integrations = []domain.Integration{{Meta: domain.Meta{ID: "i1", ProductID: "p", FeatureID: "f"}, Status: "planned", Assignee: "human/A"}, {Meta: domain.Meta{ID: "i2", ProductID: "p", FeatureID: "f"}, Status: "planned", Assignee: "human/B"}}
		st.IntegrationExecutions = []domain.IntegrationExecution{{ID: "ex", IntegrationID: "i1", ProductID: "p", DesiredState: "ready", State: "running", Fence: 1, LeaseTTLSeconds: 300}}
		st.ExecutionEvents = []domain.ExecutionEvent{{ID: "ev", ProductID: "p", IntegrationID: "i1", CreatedAt: now, Delivery: domain.ExecutionEventDelivery{Attempts: 1, DeliveryID: "d", NextAttemptAt: now.Add(time.Hour)}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(store)
	b1 := auth.WithPrincipal(ctx, auth.Principal{ID: "agent/b1", Owner: "human/B", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}})
	for _, action := range []string{"heartbeat_execution", "takeover_execution", "record_progress", "transition_integration", "ack_execution_event", "claim_execution_event_delivery"} {
		data := map[string]any{}
		integrationID := "i1"
		switch action {
		case "heartbeat_execution":
			data["fence"] = float64(1)
		case "takeover_execution":
			data = map[string]any{"orchestrator_id": "ob", "agent_id": "b1", "fence": float64(1), "state_check_reference": "ev"}
		case "record_progress":
			data = map[string]any{"title": "foreign", "completed": []string{"work"}}
		case "transition_integration":
			data = map[string]any{"status": "working"}
		case "ack_execution_event":
			data = map[string]any{"event_id": "ev", "delivery_id": "d", "supervisor_id": "sup", "outcome": "delivered"}
		case "claim_execution_event_delivery":
			data = map[string]any{"event_id": "ev", "supervisor_id": "sup"}
		}
		if strings.Contains(action, "event") {
			integrationID = ""
		}
		_, err = svc.Execute(b1, domain.Command{Action: action, Actor: "agent", ProductID: "p", IntegrationID: integrationID, Data: data})
		if err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
			t.Fatalf("%s should be rejected for human B, got %v", action, err)
		}
	}
	value, err := svc.Query(b1, "list_execution_events", `{"product_id":"p"}`)
	if err != nil || len(value.(map[string]any)["items"].([]domain.ExecutionEvent)) != 0 {
		t.Fatalf("B should not see A's event: %#v, %v", value, err)
	}
	_, err = svc.Execute(auth.WithPrincipal(ctx, auth.Principal{ID: "agent/a1", Owner: "human/A", Kind: "agent", Role: "member", Products: []string{"p"}, Permissions: []string{"read", "write"}}), domain.Command{Action: "create_execution_group", Actor: "agent", ProductID: "p", Data: map[string]any{"name": "mixed", "orchestrator_id": "oa", "integration_ids": []string{"i1", "i2"}}})
	if err == nil || !strings.Contains(err.Error(), "NOT_ASSIGNEE") {
		t.Fatalf("mixed-owner group should fail, got %v", err)
	}
}

func TestPostgresExecutionLifecycle(t *testing.T) {
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
	schema := fmt.Sprintf("execution_lifecycle_test_%d", time.Now().UnixNano())
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
	runExecutionLifecycle(t, store)
}
