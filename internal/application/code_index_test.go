package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

func TestCodeIndexMemory(t *testing.T) {
	ctx := context.Background()
	service := New(&memoryStore{state: domain.EmptyState()})
	exec(t, service, domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "Product"}})
	exec(t, service, domain.Command{Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "Repo", "url": "https://example.invalid/r"}})
	doc := domain.CodeIndex{RepositoryID: "r", Branch: "main", IndexedAt: time.Now(), Version: domain.CodeIndexVersion, Files: []domain.CodeFile{
		{Path: "a.go", Language: "Go", Size: 100, Lines: 1, Symbols: []domain.CodeSymbol{{Name: "Other", Kind: "type", Doc: "uses Main", Line: 1}}},
		{Path: "b.go", Language: "Go", Size: 100, Lines: 1, Symbols: []domain.CodeSymbol{{Name: "Main", Kind: "function", Line: 1}}},
	}}
	for sequence := 0; sequence < 23; sequence++ {
		doc.Commit = fmt.Sprintf("%040x", sequence+1)
		if out, err := service.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: "p", RepositoryID: "r", Document: doc}); err != nil || !out.Changed {
			t.Fatalf("upsert: %+v %v", out, err)
		}
	}
	for _, record := range service.codeMemory.records {
		if len(record.history) != domain.CodeHistorySize {
			t.Fatal("uncapped history")
		}
	}
	doc.Files[1].Symbols[0].Name = "NotStored"
	result, err := service.SearchCode(ctx, domain.CodeSearchRequest{ProductID: "p", Kind: "symbol", Query: "main", Limit: 1})
	if err != nil || len(result.Items) != 1 || result.Items[0].Path != "b.go" || !result.Truncated {
		t.Fatalf("search: %+v %v", result, err)
	}
}
