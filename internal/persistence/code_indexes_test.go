package persistence

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
)

type memoryCodeIndexBackend struct{ application.Store }

func TestCodeIndexRetentionAndSnippetParity(t *testing.T) {
	var baseline map[string]domain.CodeSearchResult
	for _, backend := range []string{"memory", "sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				local, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "index.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(local.Close)
				store = local
				if backend == "memory" {
					store = memoryCodeIndexBackend{local}
				}
			}
			if err := store.Update(ctx, func(st *domain.State) error {
				*st = retrievalFixture()
				st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "second", ProductID: "p"}, Name: "Second", Role: "APPLICATION", URL: "https://hidden-user:hidden-password@EXAMPLE.invalid:443/second.git?sig=hidden-signature"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			service := application.New(store)
			page, err := service.ListCodeRepositories(ctx, "p", "")
			if err != nil || len(page.Items) != 2 || page.Items[1] != (domain.CodeRepository{ID: "second", Identity: "example.invalid/second"}) {
				t.Fatalf("credential-free repository projection: %+v, %v", page, err)
			}
			for _, cloneURL := range []string{"https://user@example.invalid/repo", "https://example.invalid/repo?sig=hidden", "ssh://git:fixture@example.invalid/repo"} {
				if _, err := service.Execute(ctx, domain.Command{Action: "create_repository", ProductID: "p", Actor: "fixture", Data: map[string]any{"name": "unsafe", "url": cloneURL}}); !errors.Is(err, application.ErrValidation) || !strings.Contains(err.Error(), "clone URL") {
					t.Fatalf("credential-bearing clone URL accepted: %v", err)
				}
			}
			commit := strings.Repeat("a", 40)
			put := func(repo, branch string, files []domain.CodeFile) {
				t.Helper()
				doc := domain.CodeIndex{RepositoryID: repo, Branch: branch, Commit: commit, IndexedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), Version: domain.CodeIndexVersion, Files: files}
				if result, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "p", RepositoryID: repo, Document: doc}); err != nil || !result.Changed {
					t.Fatalf("index %s/%s: %+v, %v", repo, branch, result, err)
				}
			}
			put("second", "stable", []domain.CodeFile{})
			for index := 0; index < domain.CodeBranchLimit; index++ {
				put("r", fmt.Sprintf("branch-%03d", index), []domain.CodeFile{{Path: "README.md", Language: "Markdown"}})
			}
			longDoc := "\n\nlong needle " + strings.Repeat("界", 200) + "  "
			put("r", "newest", []domain.CodeFile{
				{Path: "README.md", Language: "Markdown", Size: len(longDoc), Lines: 3, Doc: longDoc},
				{Path: "docs/short.md", Language: "Markdown", Size: 30, Lines: 3, Doc: "\n\n short needle \n"},
			})
			status, err := service.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: "p"})
			if err != nil || status.Truncated || len(status.Items) != 2 || status.Items[0].Branch != "newest" || status.Items[1].Branch != "stable" {
				t.Fatalf("product status: %+v, %v", status, err)
			}
			status, err = service.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: "p", RepositoryID: "r"})
			if err != nil || len(status.Items) != domain.CodeBranchLimit || status.Items[0].Branch != "newest" {
				t.Fatalf("repository status: %+v, %v", status, err)
			}
			if _, err := service.GetCodeMap(ctx, domain.CodeMapRequest{ProductID: "p", RepositoryID: "r", Branch: "branch-000"}); !errors.Is(err, application.ErrNotFound) {
				t.Fatalf("evicted snapshot: %v", err)
			}
			if transactional, ok := store.(codeTransactions); ok {
				if err := transactional.codeTransaction(ctx, false, func(tx stateTransaction) error {
					var count int
					err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM code_index_history WHERE branch='branch-000')+(SELECT count(*) FROM code_index_cards WHERE branch='branch-000')`).Scan(&count)
					if err == nil && count != 0 {
						return fmt.Errorf("evicted branch left %d cards or receipts", count)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			results := map[string]domain.CodeSearchResult{}
			for query, want := range map[string]string{"short needle": "short needle", "long needle": "long needle " + strings.Repeat("界", 156), "needle": "needle " + strings.Repeat("界", 157)} {
				found, err := service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: "p", Kind: "doc", Query: query})
				if err != nil || len(found.Items) == 0 || found.Items[0].Snippet != want {
					t.Fatalf("search %q: %+v, %v", query, found, err)
				}
				results[query] = found
			}
			if backend == "memory" {
				baseline = results
			} else if !reflect.DeepEqual(results, baseline) {
				t.Fatalf("snippets differ from memory: %+v vs %+v", results, baseline)
			}
		})
	}
}

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
			query := domain.CodeSearchRequest{ProductID: "p", Query: "no-such-match", Limit: 1}
			if cards, err := findCodeWithinBudget(ctx, store, query, 2); !errors.Is(err, domain.ErrCodeSearchBudget) || len(cards) != 0 {
				t.Fatalf("no-match query exceeded its candidate budget: %+v, %v", cards, err)
			}
			query.Kind = "route"
			if cards, err := findCodeWithinBudget(ctx, store, query, 1); err != nil || len(cards) != 0 {
				t.Fatalf("no-match query exactly at its candidate budget: %+v, %v", cards, err)
			}
			query.Query = "ead"
			if cards, err := findCodeWithinBudget(ctx, store, query, 1); err != nil || len(cards) != 1 || cards[0].Symbol != "/ready" {
				t.Fatalf("substring fallback: %+v, %v", cards, err)
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

func TestLargeProductCodeSearchPreservesIndexedAndAuthoredResults(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			if backend == "postgres" {
				store = retrievalPostgres(t)
			} else {
				local, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "index.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(local.Close)
				store = local
				if backend == "memory" {
					store = memoryCodeIndexBackend{local}
				}
			}
			if err := store.Update(ctx, func(st *domain.State) error {
				*st = retrievalFixture()
				st.Features[0].Title = "NeedleFallback"
				for index := 1; index <= 4; index++ {
					repo := fmt.Sprintf("repo-%d", index)
					st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: repo, ProductID: "p"}, Name: repo, Role: "APPLICATION", URL: "https://example.invalid/" + repo})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			service := application.New(store)
			files := make([]domain.CodeFile, 9000)
			for index := range files {
				files[index] = domain.CodeFile{Path: fmt.Sprintf("file-%04d.go", index), Language: "Go"}
			}
			for index := 1; index <= 4; index++ {
				repo := fmt.Sprintf("repo-%d", index)
				if index == 4 {
					files[0].Size, files[0].Lines = 10, 1
					files[0].Symbols = []domain.CodeSymbol{{Name: "ResolveCredential", Kind: "function", Line: 1}}
				}
				doc := domain.CodeIndex{RepositoryID: repo, Branch: "main", Commit: strings.Repeat("a", 40), IndexedAt: time.Now(), Version: domain.CodeIndexVersion, Files: files}
				if _, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "p", RepositoryID: repo, Document: doc}); err != nil {
					t.Fatal(err)
				}
			}
			found, err := service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: "p", Query: "ResolveCredential", Limit: 1})
			if err != nil || len(found.Items) != 1 || found.Items[0].RepositoryID != "repo-4" || found.Items[0].Symbol != "ResolveCredential" || found.Truncated {
				t.Fatalf("indexed search over scan budget: %+v, %v", found, err)
			}
			if backend == "memory" {
				if _, err := service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: "p", Query: "NeedleFallback"}); !errors.Is(err, application.ErrValidation) || !strings.Contains(err.Error(), "scan limit exceeded") {
					t.Fatalf("memory fallback did not enforce the scan budget: %v", err)
				}
				return
			}
			query := domain.ContextSearchRequest{ProductID: "p", Query: "NeedleFallback"}
			authored, err := service.SearchContext(ctx, query)
			if err != nil || len(authored.Items) == 0 {
				t.Fatalf("authored search: %+v, %v", authored, err)
			}
			query.IncludeCode = true
			mixed, err := service.SearchContext(ctx, query)
			if err != nil || !reflect.DeepEqual(mixed.Items, authored.Items) || mixed.CodeWarning == "" || len(mixed.CodeItems) != 0 {
				t.Fatalf("fallback erased authored results: %+v, %v", mixed, err)
			}
		})
	}
}
