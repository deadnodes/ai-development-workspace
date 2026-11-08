package application

import (
	"context"
	"encoding/json"
	"errors"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
)

type retrievalOnlyStore struct {
	calls   int
	foreign bool
}

func (*retrievalOnlyStore) Read(context.Context) (domain.State, error) {
	panic("retrieval must not call full Read")
}
func (*retrievalOnlyStore) Update(context.Context, func(*domain.State) error) error {
	panic("read-only retrieval must not write")
}
func (s *retrievalOnlyStore) SearchContext(_ context.Context, q domain.ContextSearchRequest) (domain.ContextSearchResult, error) {
	s.calls++
	p := q.ProductID
	if s.foreign {
		p = "foreign"
	}
	return domain.ContextSearchResult{Items: []domain.ContextCard{{ID: "f", Kind: "features", ProductID: p, Snippet: "matching text"}}}, nil
}
func (s *retrievalOnlyStore) ContextExcerpt(_ context.Context, q domain.ContextExcerptRequest) (domain.ContextExcerpt, error) {
	s.calls++
	return domain.ContextExcerpt{ContextCard: domain.ContextCard{ID: q.ID, ProductID: q.ProductID}, Text: "excerpt"}, nil
}
func (s *retrievalOnlyStore) GraphNeighbors(_ context.Context, q domain.GraphNeighborsRequest) (domain.GraphNeighborsResult, error) {
	s.calls++
	return domain.GraphNeighborsResult{Seed: domain.ContextCard{ID: q.ID, ProductID: q.ProductID}, Nodes: []domain.ContextCard{{ID: "i", ProductID: q.ProductID}}, Edges: []domain.ContextEdge{{Source: q.ID, Target: "i", Relation: "belongs_to_feature"}}}, nil
}
func TestContextRetrievalAuthorizationAndLimits(t *testing.T) {
	store := &retrievalOnlyStore{}
	service := New(store)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/test", Kind: "agent", Permissions: []string{"read"}, Products: []string{"p"}})
	if _, e := service.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "q", Query: "query"}); !errors.Is(e, auth.ErrForbidden) || store.calls != 0 {
		t.Fatal("foreign product reached reader", e)
	}
	for _, q := range []domain.ContextSearchRequest{{ProductID: "p", Query: ""}, {ProductID: "p", Query: "q", Limit: 21}, {ProductID: "p", Query: strings.Repeat("я", 257)}, {ProductID: "p", Query: "q", FeatureID: "invalid id"}} {
		if _, e := service.SearchContext(ctx, q); e == nil {
			t.Fatal("invalid search accepted")
		}
	}
	if _, e := service.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: "p", ID: "f", MaxChars: 8001}); e == nil {
		t.Fatal("oversize excerpt accepted")
	}
	if _, e := service.GraphNeighbors(ctx, domain.GraphNeighborsRequest{ProductID: "p", ID: "f", Depth: 3}); e == nil {
		t.Fatal("oversize depth accepted")
	}
	r, e := service.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "query"})
	if e != nil || len(r.Items) != 1 {
		t.Fatal(e)
	}
	if _, e = service.ContextExcerpt(ctx, domain.ContextExcerptRequest{ProductID: "p", ID: "f"}); e != nil {
		t.Fatal(e)
	}
	if _, e = service.GraphNeighbors(ctx, domain.GraphNeighborsRequest{ProductID: "p", ID: "f"}); e != nil {
		t.Fatal(e)
	}
	store.foreign = true
	if _, e = service.SearchContext(ctx, domain.ContextSearchRequest{ProductID: "p", Query: "query"}); e == nil {
		t.Fatal("bad reader scope accepted")
	}
	b, _ := json.Marshal(r)
	if len(b) > ContextRetrievalMaxBytes {
		t.Fatal("budget")
	}
}
