package application

import (
	"context"
	"encoding/json"
	"fmt"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"unicode/utf8"
)

type pageStore struct {
	cards []domain.StateCard
	calls int
	query domain.StateMetadataRequest
}

func (s *pageStore) Read(context.Context) (domain.State, error) { panic("global Read forbidden") }
func (s *pageStore) Update(context.Context, func(*domain.State) error) error {
	panic("Update forbidden")
}
func (s *pageStore) ReadStateMetadata(_ context.Context, q domain.StateMetadataRequest) (domain.StateMetadata, error) {
	s.calls++
	s.query = q
	out := domain.StateMetadata{Product: &domain.StateCard{ID: "p"}, Feature: &domain.StateCard{ID: "f", ProductID: "p", RecordedStatus: "planned"}}
	for _, c := range s.cards {
		if c.ID > q.AfterID && len(out.Items) < q.Limit {
			out.Items = append(out.Items, c)
		}
	}
	return out, nil
}

type noPageStore struct{}

func (noPageStore) Read(context.Context) (domain.State, error) { panic("fallback forbidden") }
func (noPageStore) Update(context.Context, func(*domain.State) error) error {
	panic("fallback forbidden")
}
func TestStatePageNoRawFallback(t *testing.T) {
	if _, e := New(noPageStore{}).StatePage(context.Background(), domain.StatePageRequest{}); e == nil {
		t.Fatal("missing capability accepted")
	}
}
func TestStatePageBudgetCursorAndUnicode(t *testing.T) {
	st := &pageStore{}
	for i := 0; i < 100; i++ {
		st.cards = append(st.cards, domain.StateCard{ID: fmt.Sprintf("%03d%s", i, strings.Repeat("x", 125)), Name: strings.Repeat("\x01", 256), Title: strings.Repeat("界", 200)})
	}
	s := New(st)
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, e := s.StatePage(context.Background(), domain.StatePageRequest{Limit: 100, Cursor: cursor})
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > StatePageMaxBytes {
			t.Fatalf("%d bytes", len(raw))
		}
		if len(page.Items) == 0 {
			t.Fatal("empty page")
		}
		for _, c := range page.Items {
			if seen[c.ID] {
				t.Fatal("duplicate")
			}
			seen[c.ID] = true
			if len(c.Title) > 256 || !utf8.ValidString(c.Title) || !c.Truncated {
				t.Fatal("bad Unicode bound")
			}
		}
		pages++
		if !page.Pagination.HasMore {
			break
		}
		if page.Pagination.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.Pagination.NextCursor
	}
	if len(seen) != 100 || pages < 2 {
		t.Fatalf("seen=%d pages=%d", len(seen), pages)
	}
}
func TestStatePageScopeAndInput(t *testing.T) {
	st := &pageStore{}
	s := New(st)
	for _, in := range []domain.StatePageRequest{{ProductID: " "}, {ProductID: "p p"}, {ProductID: strings.Repeat("界", 50)}, {Limit: 101}, {Limit: -1}, {Cursor: "bad"}, {Cursor: strings.Repeat("a", 2049)}} {
		if _, e := s.StatePage(context.Background(), in); e == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
	if st.calls != 0 {
		t.Fatal("invalid input accessed store")
	}
	if _, e := s.StatePage(context.Background(), domain.StatePageRequest{ProductID: "other", FeatureID: "f"}); e == nil {
		t.Fatal("cross product accepted")
	}
	page, e := s.StatePage(context.Background(), domain.StatePageRequest{FeatureID: "f"})
	if e != nil || page.Scope.Product.ID != "p" {
		t.Fatalf("feature scope %v", e)
	}
	cur := encodeStateCursor(domain.StateMetadataRequest{Collection: "products"}, "p")
	if _, e := s.StatePage(context.Background(), domain.StatePageRequest{ProductID: "p", Cursor: cur}); e == nil {
		t.Fatal("cross scope cursor accepted")
	}
	st.cards = []domain.StateCard{{ID: strings.Repeat("i", 129)}}
	if _, e := s.StatePage(context.Background(), domain.StatePageRequest{}); e == nil {
		t.Fatal("oversized identity truncated")
	}
}
