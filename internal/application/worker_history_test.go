package application

import (
	"context"
	"errors"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

type historySpyStore struct {
	noPageStore
	calls int
	cards []domain.WorkerHistoryItem
}

func (s *historySpyStore) ReadWorkerHistory(_ context.Context, q domain.WorkerHistoryMetadataRequest) (domain.WorkerHistoryMetadata, error) {
	s.calls++
	out := domain.WorkerHistoryMetadata{ProductFound: true, FeatureFound: true, JobFound: true}
	for _, c := range s.cards {
		if c.ID > q.AfterID && len(out.Items) < q.Limit {
			out.Items = append(out.Items, c)
		}
	}
	return out, nil
}
func TestWorkerHistoryAuthorizationAndNoFallback(t *testing.T) {
	store := &historySpyStore{}
	service := New(store)
	q := domain.WorkerHistoryRequest{ProductID: "p"}
	if _, e := service.WorkerHistory(agentContext("write"), q); !errors.Is(e, auth.ErrForbidden) || store.calls != 0 {
		t.Fatal("permission checked after IO", e)
	}
	q.ProductID = "private"
	if _, e := service.WorkerHistory(agentContext("read"), q); !errors.Is(e, auth.ErrForbidden) || store.calls != 0 {
		t.Fatal("scope checked after IO", e)
	}
	q.ProductID = "p"
	if _, e := New(noPageStore{}).WorkerHistory(context.Background(), q); e == nil {
		t.Fatal("fallback accepted")
	}
}
func TestWorkerHistoryPagingScopeAndBudget(t *testing.T) {
	store := &historySpyStore{}
	for i := 0; i < 100; i++ {
		store.cards = append(store.cards, domain.WorkerHistoryItem{ID: fmt.Sprintf("%03d%s", i, strings.Repeat("x", 125)), ProductID: "p", OperationID: strings.Repeat("o", 128), SourceSHA: strings.Repeat("s", 128), RecipeHash: strings.Repeat("h", 128), InputHash: strings.Repeat("i", 128)})
	}
	service := New(store)
	q := domain.WorkerHistoryRequest{ProductID: "p", FeatureID: "f", Limit: 100}
	first, e := service.WorkerHistory(context.Background(), q)
	if e != nil || !first.Pagination.HasMore || len(first.Items) == 0 || len(first.Items) >= 100 {
		t.Fatal("byte budget not enforced", e, len(first.Items))
	}
	q.Cursor = first.Pagination.NextCursor
	q.JobID = "other"
	before := store.calls
	if _, e = service.WorkerHistory(context.Background(), q); e == nil || store.calls != before {
		t.Fatal("cross job cursor accepted")
	}
	q.JobID = ""
	q.FeatureID = "other"
	if _, e = service.WorkerHistory(context.Background(), q); e == nil || store.calls != before {
		t.Fatal("cross Feature cursor accepted")
	}
	q.FeatureID = "f"
	q.Collection = "links"
	q.JobID = "j"
	if _, e = service.WorkerHistory(context.Background(), q); e == nil || store.calls != before {
		t.Fatal("cross collection cursor accepted")
	}
	q.Collection = "jobs"
	q.JobID = ""
	second, e := service.WorkerHistory(context.Background(), q)
	if e != nil || second.Items[0].ID <= first.Items[len(first.Items)-1].ID {
		t.Fatal("paging lost ordering", e)
	}
	q.Cursor = ""
	store.cards[0].ID = strings.Repeat("x", 129)
	if _, e = service.WorkerHistory(context.Background(), q); e == nil {
		t.Fatal("oversized exact ID truncated")
	}
	for _, bad := range []domain.WorkerHistoryRequest{{}, {ProductID: "p", Collection: "attempts"}, {ProductID: "p", Limit: 101}, {ProductID: "p", Collection: "logs"}} {
		if _, e = service.WorkerHistory(context.Background(), bad); e == nil {
			t.Fatal("bad request accepted", bad)
		}
	}
}

func TestWorkerHistoryCompleteByteSplitLinksAndCursorTampering(t *testing.T) {
	store := &historySpyStore{}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("%03d%s", i, strings.Repeat("x", 125))
		store.cards = append(store.cards, domain.WorkerHistoryItem{ID: id, RevisionID: id, ProductID: "p", JobID: "j", FeatureID: strings.Repeat("f", 128), IntegrationID: strings.Repeat("i", 128), RepositoryID: strings.Repeat("r", 128), SourceSHA: strings.Repeat("s", 128), Provenance: "recorded_snapshot_unverified"})
	}
	service := New(store)
	q := domain.WorkerHistoryRequest{ProductID: "p", JobID: "j", Collection: "links", Limit: 100}
	seen := map[string]bool{}
	pages := 0
	for {
		out, e := service.WorkerHistory(context.Background(), q)
		if e != nil {
			t.Fatal(e)
		}
		pages++
		for _, c := range out.Items {
			if seen[c.ID] {
				t.Fatal("duplicate link")
			}
			seen[c.ID] = true
		}
		if !out.Pagination.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("paging did not progress")
		}
		q.Cursor = out.Pagination.NextCursor
		if pages == 1 {
			bad := q
			bad.Cursor += "="
			before := store.calls
			if _, e = service.WorkerHistory(context.Background(), bad); e == nil || store.calls != before {
				t.Fatal("noncanonical cursor accepted")
			}
			bad = q
			bad.ProductID = "other"
			if _, e = service.WorkerHistory(context.Background(), bad); e == nil || store.calls != before {
				t.Fatal("cross product cursor accepted")
			}
		}
	}
	if pages < 2 || len(seen) != 100 {
		t.Fatal("byte paging omitted links", pages, len(seen))
	}
}
