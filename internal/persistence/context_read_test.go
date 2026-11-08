package persistence

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresContextReadExcludesHistoryAndBoundsText(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not configured; PostgreSQL not verified")
	}
	u, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(u.Path, "_tests") {
		t.Fatal("dedicated *_tests DB required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	schema := fmt.Sprintf("context_read_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	store, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.pool.Exec(ctx, `INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES
 ('p','products','','','{"id":"p","name":"Product"}',0),
 ('f','features','p','','{"id":"f","product_id":"p","title":"Feature","status":"active"}',0),
 ('i','integrations','p','f',jsonb_build_object('id','i','product_id','p','feature_id','f','title','Integration','status','planned','objective',repeat('x',100000)),0),
 ('m','memories','p','f',jsonb_build_object('id','m','product_id','p','feature_id','f','kind','handoff','body',repeat('y',100000),'completed',jsonb_build_array(repeat('z',100000),'done'),'next',jsonb_build_array('next-action')),0),
 ('k1','project_knowledge','p','','{"id":"k1","product_id":"p","overview":"old"}',1),
 ('k2','project_knowledge','p','','{"id":"k2","product_id":"p","overview":"latest"}',2),
 ('poison','operations','p','','{"status":"invalid"}',0);
 DROP TABLE events;`)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.ReadContext(ctx, "", "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Features) != 1 || len(st.Integrations) != 1 || len(st.Memories) != 1 {
		t.Fatal("missing selected feature context")
	}
	if len(st.Integrations[0].Objective) > 600 || len(st.Memories[0].Body) > 800 {
		t.Fatal("large text crossed projection")
	}
	if len(st.Memories[0].Completed) != 2 || len(st.Memories[0].Completed[0]) > 256 || len(st.Memories[0].Next) != 1 {
		t.Fatal("structured checkpoint fields missing or unbounded")
	}
	if len(st.Operations) != 0 || len(st.Events) != 0 {
		t.Fatal("history leaked")
	}
	st, err = store.ReadContext(ctx, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Products) != 1 || len(st.ProjectKnowledge) != 1 || st.ProjectKnowledge[0].Overview != "latest" {
		t.Fatal("project context mismatch")
	}
	st, err = store.ReadContext(ctx, "", "missing")
	if err != nil || len(st.Features) != 0 {
		t.Fatal("missing feature did not stay empty", err)
	}
}
