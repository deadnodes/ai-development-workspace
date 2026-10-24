package persistence

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"releasecontrol/internal/domain"
)

func TestPostgresProductDeletionPreservesOtherOwnersAndRollsBack(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL required")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_tests") {
		t.Fatal("dedicated *_tests database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	schema := pgx.Identifier{fmt.Sprintf("delete_batch_%d", time.Now().UnixNano())}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, e := conn.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); e != nil {
			t.Error(e)
		}
	}()
	q := u.Query()
	q.Set("search_path", strings.Trim(schema, `"`))
	u.RawQuery = q.Encode()
	s, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Update(ctx, func(st *domain.State) error {
		st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Delete"}, {Meta: domain.Meta{ID: "q"}, Name: "Keep"}}
		for _, owner := range []string{"p", "q"} {
			for i := 0; i < 3; i++ {
				st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprintf("%s-%d", owner, i), ProductID: owner}, Kind: "REFRESH_GIT", Status: "SUCCEEDED"})
			}
		}
		st.Events = []domain.Event{{ID: "audit-p", ProductID: "p", Action: "record_progress"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remove := func(st *domain.State) error {
		st.Products = st.Products[1:]
		st.Operations = st.Operations[3:]
		return nil
	}
	// Failure after the delete statements must roll back the entire transaction.
	if _, err = s.pool.Exec(ctx, "ALTER TABLE events ADD CONSTRAINT reject_fixture_event CHECK (id <> 'reject-event')"); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(ctx, func(st *domain.State) error {
		_ = remove(st)
		st.Events = append(st.Events, domain.Event{ID: "reject-event", ProductID: "q", Action: "record_progress"})
		return nil
	}); err == nil {
		t.Fatal("expected event constraint failure")
	}
	st, err := s.Read(ctx)
	if err != nil || len(st.Products) != 2 || len(st.Operations) != 6 || len(st.Events) != 1 {
		t.Fatal("failed deletion did not roll back", err)
	}
	if err = s.Update(ctx, remove); err != nil {
		t.Fatal(err)
	}
	st, err = s.Read(ctx)
	if err != nil || len(st.Products) != 1 || st.Products[0].ID != "q" || len(st.Operations) != 3 || len(st.Events) != 1 || st.Events[0].ID != "audit-p" {
		t.Fatal("deletion altered retained owner/audit", err)
	}
	for _, op := range st.Operations {
		if op.ProductID != "q" {
			t.Fatal("deleted owner's record retained")
		}
	}
}
