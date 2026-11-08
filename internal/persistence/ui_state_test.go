package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"os"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in read-only measurement against the operator's existing dataset.
func TestUIStateLargeInstallationReadOnly(t *testing.T) {
	url := os.Getenv("RCP_UI_READONLY_DATABASE_URL")
	if url == "" {
		t.Skip("RCP_UI_READONLY_DATABASE_URL not supplied")
	}
	pool, e := pgxpool.New(context.Background(), url)
	s := &Store{pool: pool}
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	start := time.Now()
	out, e := s.ReadUIState(context.Background(), domain.UIStateRequest{ProductID: "deadnodes"})
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(out)
	if e != nil {
		t.Fatal(e)
	}
	var sizes map[string]json.RawMessage
	_ = json.Unmarshal(b, &sizes)
	for k, v := range sizes {
		if len(v) > 100000 {
			t.Logf("%s=%d bytes", k, len(v))
		}
	}
	t.Logf("UI projection: %s, %d bytes, %d products, %d features, %d operations", time.Since(start), len(b), len(out.Products), len(out.Features), len(out.Operations))
	if len(out.RuntimeObservations) != 0 || len(out.Events) != 0 || len(out.Memories) != 0 {
		t.Fatal("unrequested raw history loaded")
	}
	if len(b) > 8<<20 {
		t.Fatal("oversize browser projection")
	}
}

func TestUIBudgetIncludesAppendedHistory(t *testing.T) {
	groups := map[string][]json.RawMessage{"products": {json.RawMessage(`{"id":"p","name":"P"}`)}, "events": {json.RawMessage(`{"id":"e","data":"` + strings.Repeat("x", 8<<20) + `"}`)}}
	p := uiProjection(domain.UIStateRequest{ProductID: "p"})
	if e := budgetUIGroups(groups, &p); e != nil {
		t.Fatal(e)
	}
	if len(groups["events"]) != 0 || len(groups["products"]) != 1 || !slices.Contains(p.Truncated, "events") {
		t.Fatal("appended audit escaped budget or mutable record removed")
	}
}

func TestPostgresUIHeadersAndOperationAppendOrder(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured")
	}
	u, e := url.Parse(dsn)
	if e != nil || !strings.HasSuffix(u.Path, "_tests") {
		t.Fatal("dedicated *_tests database required")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	schema := fmt.Sprintf("ui_read_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE SCHEMA "+quoted); e != nil {
		t.Fatal(e)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	s, e := Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	_, e = s.pool.Exec(ctx, `INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES
 ('p','products','','','{"id":"p","name":"Product"}',0),
 ('other','products','','',jsonb_build_object('id','other','name','Other','description',repeat('x',9000000)),1),
 ('f','features','p','',jsonb_build_object('id','f','product_id','p','title','Selected','status','active','goal','Editable goal'),0),
 ('foreign','features','other','',jsonb_build_object('id','foreign','product_id','other','title','Foreign navigation','status','active','context',repeat('x',9000000)),1),
 ('refresh','operations','p','',jsonb_build_object('id','refresh','product_id','p','kind','REFRESH_GIT','status','SUCCEEDED'),300);
 INSERT INTO records(id,kind,product_id,feature_id,document,position) SELECT 'op'||lpad(n::text,3,'0'),'operations','p','',jsonb_build_object('id','op'||lpad(n::text,3,'0'),'product_id','p','kind','DEPLOY','status','SUCCEEDED'),n FROM generate_series(0,249) AS n;`)
	if e != nil {
		t.Fatal(e)
	}
	out, e := s.ReadUIState(ctx, domain.UIStateRequest{ProductID: "p", FeatureID: "f"})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(out)
	if len(b) > 100000 {
		t.Fatalf("unrelated full documents leaked: %d bytes", len(b))
	}
	for _, f := range out.Features {
		if f.ID == "f" && f.Goal != "Editable goal" {
			t.Fatal("selected editable feature lost full fields")
		}
		if f.ID == "foreign" && f.Context != "" {
			t.Fatal("foreign navigation includes full text")
		}
	}
	if len(out.Operations) != 201 || out.Operations[0].ID != "op050" || out.Operations[len(out.Operations)-1].ID != "refresh" {
		t.Fatal("newest window not restored to source append order")
	}
	latest := ""
	for _, o := range out.Operations {
		if o.Kind == "DEPLOY" {
			latest = o.ID
		}
	}
	if latest != "op249" {
		t.Fatal("UI reverse/slice would show old operation")
	}
}
