package application

import (
	"context"
	"encoding/json"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"testing"
	"unicode/utf8"
)

type summaryContextStore struct {
	fullReads int
	st        domain.State
}

func (s *summaryContextStore) Read(context.Context) (domain.State, error) {
	s.fullReads++
	return s.st, nil
}
func (s *summaryContextStore) Update(context.Context, func(*domain.State) error) error { return nil }
func (s *summaryContextStore) ReadContext(context.Context, string, string) (domain.State, error) {
	return s.st, nil
}
func TestResumeSummaryBoundedAndScoped(t *testing.T) {
	st := domain.EmptyState()
	st.Features = append(st.Features, domain.Feature{Meta: domain.Meta{ID: "f", ProductID: "p"}, Title: strings.Repeat("界", 10000), Goal: strings.Repeat("界", 10000)})
	for n := 0; n < 150; n++ {
		st.Integrations = append(st.Integrations, domain.Integration{Meta: domain.Meta{ID: strings.Repeat("i", 100), FeatureID: "f", ProductID: "p"}, Title: strings.Repeat("界", 10000), Objective: strings.Repeat("界", 10000)})
	}
	for n := 0; n < 25; n++ {
		st.Memories = append(st.Memories, domain.Memory{Meta: domain.Meta{ID: "memory", FeatureID: "f", ProductID: "p"}, Body: strings.Repeat("界", 10000), Completed: []string{strings.Repeat("界", 10000)}, Next: []string{"next-action"}})
	}
	store := &summaryContextStore{st: st}
	out, err := New(store).ResumeSummary(context.Background(), "f")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > 32<<10 {
		t.Fatalf("summary %d bytes", len(raw))
	}
	if store.fullReads != 0 {
		t.Fatal("read full store")
	}
	m := out.(map[string]any)
	if m["projection"] != "bounded_feature_summary" || m["integrations_truncated"] != true || m["memories_truncated"] != true {
		t.Fatalf("missing explicit projection bounds: %v", m)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Kind: "agent", ID: "a", Permissions: []string{"read"}, Products: []string{"other"}})
	if _, err = New(store).ResumeSummary(ctx, "f"); err == nil {
		t.Fatal("cross-product feature exposed")
	}
}

func TestProjectContextSummaryBoundedAndNoDocuments(t *testing.T) {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}}
	st.ProjectKnowledge = []domain.ProjectKnowledge{{Meta: domain.Meta{ProductID: "p"}, Overview: strings.Repeat("界", 10000), Instructions: strings.Repeat("界", 10000), Parameters: map[string]string{"control_plane.installed_commit": "abc", "control_plane.installation": strings.Repeat("界", 10000)}}}
	for n := 0; n < 100; n++ {
		st.Repositories = append(st.Repositories, domain.Repository{Meta: domain.Meta{ID: "r", ProductID: "p"}, Name: strings.Repeat("界", 10000), URL: strings.Repeat("界", 10000)})
	}
	store := &summaryContextStore{st: st}
	out, err := New(store).ProjectContextSummary(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if len(b) > 32<<10 {
		t.Fatalf("project summary %d bytes", len(b))
	}
	if store.fullReads != 0 {
		t.Fatal("read full store")
	}
	m := out.(map[string]any)
	if m["documents_omitted"] != true {
		t.Fatal("document omissions not explicit")
	}
	k := m["knowledge"].(map[string]any)
	if k["parameters"].(map[string]string)["control_plane.installed_commit"] != "abc" {
		t.Fatal("installation revision lost")
	}
}

func TestKnowledgeSearchSummaryBoundedUTF8AndHTTPFull(t *testing.T) {
	st := domain.EmptyState()
	st.Products = []domain.Product{{Meta: domain.Meta{ID: "p"}, Name: "Product"}}
	k := domain.ProjectKnowledge{Meta: domain.Meta{ProductID: "p"}}
	for n := 0; n < 80; n++ {
		k.Nodes = append(k.Nodes, domain.KnowledgeNode{Meta: domain.Meta{ID: fmt.Sprintf("node-%d", n), ProductID: "p"}, Kind: "fact", Title: strings.Repeat("界", 1000), Content: strings.Repeat("界", 10000) + "needle-mark" + strings.Repeat("😀", 10000), Status: "active"})
	}
	st.ProjectKnowledge = []domain.ProjectKnowledge{k}
	store := &summaryContextStore{st: st}
	svc := New(store)
	out, err := svc.SearchKnowledgeSummary(context.Background(), "p", "needle-mark", 50)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if len(b) > 32<<10 || !utf8.Valid(b) {
		t.Fatalf("invalid/large search summary:%d", len(b))
	}
	m := out.(map[string]any)
	if m["nodes_truncated"] != true || m["content_excerpted"] != true {
		t.Fatal("truncation not explicit")
	}
	if !strings.Contains(string(b), "needle-mark") {
		t.Fatal("snippet lost matching text")
	}
	if strings.Contains(string(b), strings.Repeat("界", 1000)) {
		t.Fatal("large source document leaked")
	}
	if store.fullReads != 0 {
		t.Fatal("full state read")
	}
	full, err := svc.SearchKnowledge(context.Background(), "p", "needle-mark", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.(map[string]any)["nodes"].([]domain.KnowledgeNode)[0].Content) <= 20000 {
		t.Fatal("explicit full context lost content")
	}
}
