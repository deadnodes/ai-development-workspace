package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"time"
)

func TestNavigationMetadataStores(t *testing.T) {
	ctx := context.Background()
	local, e := OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer local.Close()
	st := domain.EmptyState()
	st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p"}, Name: strings.Repeat("界", 1000), Description: strings.Repeat("UNEXPORTED", 1<<18)})
	st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: "Feature", Status: "planned", Context: strings.Repeat("OMITTED_CONTEXT", 1<<18)})
	for _, id := range []string{"i3", "i1", "i2"} {
		st.Integrations = append(st.Integrations, domain.Integration{Meta: domain.Meta{ID: id, ProductID: "p", FeatureID: "f"}, Title: strings.Repeat("\\\"界", 1000), Status: "planned"})
	}
	if e = local.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
		t.Fatal(e)
	}
	q := domain.StateMetadataRequest{Collection: "integrations", ProductID: "p", FeatureID: "f", AfterID: "i1", Limit: 2}
	got, e := local.ReadStateMetadata(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Items) != 2 || got.Items[0].ID != "i2" || got.Items[1].ID != "i3" || len(got.Product.Name) > 1600 || !got.Product.Truncated {
		t.Fatalf("bad bounded local metadata: %+v", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "UNEXPORTED") || strings.Contains(string(b), "OMITTED_CONTEXT") || len(b) > 16000 {
		t.Fatal("large fields leaked")
	}
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			t.Skip("TEST_DATABASE_URL required for actual PostgreSQL metadata reader")
		}
		conn, e := pgx.Connect(ctx, dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer conn.Close(ctx)
		schema := fmt.Sprintf("nav_%d", time.Now().UnixNano())
		if _, e = conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); e != nil {
			t.Fatal(e)
		}
		defer conn.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		u, e := url.Parse(dsn)
		if e != nil {
			t.Fatal(e)
		}
		args := u.Query()
		args.Set("search_path", schema)
		u.RawQuery = args.Encode()
		pg, e := Open(ctx, u.String())
		if e != nil {
			t.Fatal(e)
		}
		defer pg.Close()
		if e = pg.Update(ctx, func(s *domain.State) error { *s = st; return nil }); e != nil {
			t.Fatal(e)
		}
		// Removing the isolated events table and poisoning unrelated documents proves
		// this path cannot quietly fall back to the global State reader.
		if _, e = pg.pool.Exec(ctx, `DROP TABLE events`); e != nil {
			t.Fatal(e)
		}
		if _, e = pg.pool.Exec(ctx, `INSERT INTO records(id,kind,document) VALUES('unrelated','results','{"status":"not-a-status","data":"ignored"}')`); e != nil {
			t.Fatal(e)
		}
		pgot, e := pg.ReadStateMetadata(ctx, q)
		if e != nil {
			t.Fatal(e)
		}
		for i := range got.Items {
			got.Items[i].Title = ""
			pgot.Items[i].Title = ""
		}
		got.Product.Name = ""
		pgot.Product.Name = ""
		if !reflect.DeepEqual(got, pgot) {
			t.Fatalf("metadata differs: local=%+v pg=%+v", got, pgot)
		}
		if _, e = pg.Read(ctx); e == nil {
			t.Fatal("global read unexpectedly worked without events table")
		}
	})
}
func TestNavigationScannerEscapesAndCancellation(t *testing.T) {
	value := strings.Repeat("界😀\"\\\n", 2000)
	raw, _ := json.Marshal(value)
	s, cut, e := metadataString(raw)
	if e != nil || !cut || !strings.HasPrefix(value, s) {
		t.Fatal("unsafe prefix", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e = walkStateCollection(ctx, []byte(`{"products":[]}`), "products", func(domain.StateCard) {}); e == nil {
		t.Fatal("ignored cancellation")
	}
}
