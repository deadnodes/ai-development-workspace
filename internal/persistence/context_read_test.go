package persistence

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"releasecontrol/internal/domain"
)

func TestContextReadExcludesHistoryAndBoundsText(t *testing.T) {
	ctx := context.Background()
	t.Run("sqlite", func(t *testing.T) {
		store, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "context.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		_, err = store.db.ExecContext(ctx, `INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES
 ('p','products','','','{"id":"p","name":"Product"}',0),
 ('f','features','p','','{"id":"f","product_id":"p","title":"Feature","status":"active"}',0),
 ('i','integrations','p','f',json_object('id','i','product_id','p','feature_id','f','title','Integration','status','planned','assignee','human/A','objective',?1),0),
 ('m','memories','p','f',json_object('id','m','product_id','p','feature_id','f','kind','handoff','body',?1,'completed',json_array(?1,'done'),'next',json_array('next-action')),0),
 ('k1','project_knowledge','p','','{"id":"k1","product_id":"p","overview":"old"}',1),
 ('k2','project_knowledge','p','','{"id":"k2","product_id":"p","overview":"latest"}',2),
 ('poison','operations','p','','{"status":"invalid"}',0);
 DROP TABLE events;`, strings.Repeat("界", 100000))
		if err != nil {
			t.Fatal(err)
		}
		assertContextRead(t, ctx, store)
	})
	t.Run("postgres", func(t *testing.T) {
		store := retrievalPostgres(t)
		_, err := store.pool.Exec(ctx, `INSERT INTO records(id,kind,product_id,feature_id,document,position) VALUES
	 ('p','products','','','{"id":"p","name":"Product"}',0),
	 ('f','features','p','','{"id":"f","product_id":"p","title":"Feature","status":"active"}',0),
	 ('i','integrations','p','f',jsonb_build_object('id','i','product_id','p','feature_id','f','title','Integration','status','planned','assignee','human/A','objective',repeat('x',100000)),0),
	 ('m','memories','p','f',jsonb_build_object('id','m','product_id','p','feature_id','f','kind','handoff','body',repeat('y',100000),'completed',jsonb_build_array(repeat('z',100000),'done'),'next',jsonb_build_array('next-action')),0),
	 ('k1','project_knowledge','p','','{"id":"k1","product_id":"p","overview":"old"}',1),
	 ('k2','project_knowledge','p','','{"id":"k2","product_id":"p","overview":"latest"}',2),
	 ('poison','operations','p','','{"status":"invalid"}',0);
	 DROP TABLE events;`)
		if err != nil {
			t.Fatal(err)
		}
		assertContextRead(t, ctx, store)
	})
}

func assertContextRead(t *testing.T, ctx context.Context, store interface {
	ReadContext(context.Context, string, string) (domain.State, error)
}) {
	t.Helper()
	st, err := store.ReadContext(ctx, "", "f")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Features) != 1 || len(st.Integrations) != 1 || len(st.Memories) != 1 {
		t.Fatal("missing selected feature context")
	}
	if st.Integrations[0].Assignee != "human/A" {
		t.Fatalf("missing assignee: %q", st.Integrations[0].Assignee)
	}
	if utf8.RuneCountInString(st.Integrations[0].Objective) > 600 || utf8.RuneCountInString(st.Memories[0].Body) > 800 {
		t.Fatal("large text crossed projection")
	}
	if len(st.Memories[0].Completed) != 2 || utf8.RuneCountInString(st.Memories[0].Completed[0]) > 256 || len(st.Memories[0].Next) != 1 {
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
