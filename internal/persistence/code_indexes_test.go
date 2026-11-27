package persistence

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

func TestCodeIndexStores(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store interface {
				application.Store
				codeTransactions
			}
			var database string
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				database = filepath.Join(t.TempDir(), "control.db")
				local, err := OpenSQLite(ctx, database)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(local.Close)
				store = local
			}
			if err := store.Update(ctx, func(st *domain.State) error { *st = retrievalFixture(); return nil }); err != nil {
				t.Fatal(err)
			}
			service := application.New(store)
			doc := domain.CodeIndex{RepositoryID: "r", Branch: "main", IndexedAt: time.Now(), Version: domain.CodeIndexVersion, Files: []domain.CodeFile{
				{Path: "README.md", Language: "Markdown", Size: 100, Lines: 1, Doc: "Keep the cache; warm. Unicode: ДАННЫЕ."},
				{Path: "handler.go", Language: "Go", Size: 200, Lines: 2, Symbols: []domain.CodeSymbol{{Name: "ServeHTTP", Kind: "method", Receiver: "Handler", Doc: "Serves traffic", Line: 2}}, Routes: []domain.CodeRoute{{Path: "/ready", Method: "GET", Line: 1}}},
				{Path: "pkg.go", Language: "Go", Size: 100, Lines: 1, Symbols: []domain.CodeSymbol{{Name: "Other", Kind: "function", Doc: "Calls ServeHTTP", Line: 1}}},
			}}
			var revision int64
			if err := store.codeTransaction(ctx, false, func(tx stateTransaction) error {
				return tx.QueryRow(ctx, `SELECT revision FROM control_revision WHERE id=1`).Scan(&revision)
			}); err != nil {
				t.Fatal(err)
			}
			for iteration := 0; iteration < 22; iteration++ {
				doc.Commit = fmt.Sprintf("%040x", iteration+1)
				receipt, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "p", RepositoryID: "r", Actor: "fixture", Document: doc})
				if err != nil || !receipt.Changed || receipt.Commit != doc.Commit {
					t.Fatalf("upsert %d: %+v %v", iteration, receipt, err)
				}
			}
			doc.IndexedAt = doc.IndexedAt.Add(time.Hour)
			receipt, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "p", RepositoryID: "r", Document: doc})
			if err != nil || receipt.Changed {
				t.Fatalf("replayed receipt: %+v %v", receipt, err)
			}
			if err := store.codeTransaction(ctx, false, func(tx stateTransaction) error {
				var count int
				var current int64
				var actor string
				if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM code_index_history),revision,(SELECT actor FROM code_indexes) FROM control_revision WHERE id=1`).Scan(&count, &current, &actor); err != nil {
					return err
				}
				if count != domain.CodeHistorySize || current != revision || actor != "fixture" {
					return fmt.Errorf("history=%d revision=%d want=%d actor=%q", count, current, revision, actor)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if database != "" {
				reader, err := OpenSQLiteReadOnly(ctx, database)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				service = application.New(reader)
			}
			for _, search := range []struct{ query, kind, path string }{{"servehttp", "symbol", "handler.go"}, {"cache warm", "doc", "README.md"}, {"данные", "doc", "README.md"}, {"/ready", "route", "handler.go"}} {
				result, err := service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: "p", Kind: search.kind, Query: search.query, Limit: 1})
				if err != nil || len(result.Items) != 1 || result.Items[0].Path != search.path || result.Items[0].Commit != doc.Commit || result.Truncated != (search.kind == "symbol") {
					t.Fatalf("search %q: %+v %v", search.query, result, err)
				}
			}
			status, err := service.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: "p"})
			if err != nil || len(status.Items) != 1 || status.Items[0].Commit != doc.Commit {
				t.Fatalf("status: %+v %v", status, err)
			}
			contextResult, err := service.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "servehttp", IncludeCode: true})
			if err != nil || len(contextResult.CodeItems) != 2 {
				t.Fatalf("search context: %+v %v", contextResult, err)
			}
			outline, err := service.GetCodeFileOutline(ctx, domain.CodeOutlineRequest{ProductID: "p", RepositoryID: "r", Path: "handler.go"})
			if err != nil || !strings.Contains(fmt.Sprint(outline), "/ready") {
				t.Fatalf("outline: %+v %v", outline, err)
			}
		})
	}
}
