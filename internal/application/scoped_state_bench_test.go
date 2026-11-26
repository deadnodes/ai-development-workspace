package application

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func benchmarkScopedState() domain.State {
	st := domain.EmptyState()
	st.Products = append(st.Products, domain.Product{Meta: domain.Meta{ID: "p", ProductID: "p"}}, domain.Product{Meta: domain.Meta{ID: "q", ProductID: "q"}})
	for i := 0; i < 300; i++ {
		p := "p"
		if i%2 == 1 {
			p = "q"
		}
		st.Integrations = append(st.Integrations, domain.Integration{Meta: domain.Meta{ID: fmt.Sprintf("i-%d", i), ProductID: p}, Title: "integration"})
		st.Builds = append(st.Builds, domain.Build{Meta: domain.Meta{ID: fmt.Sprintf("b-%d", i), ProductID: p}})
		st.Operations = append(st.Operations, domain.ExternalOperation{Meta: domain.Meta{ID: fmt.Sprintf("o-%d", i), ProductID: p}})
	}
	payload := strings.Repeat("x", 2048)
	for i := 0; i < 5000; i++ {
		p := "p"
		if i%2 == 1 {
			p = "q"
		}
		st.Events = append(st.Events, domain.Event{ID: fmt.Sprintf("e-%d", i), ProductID: p, Action: "record_progress", Data: domain.Command{Data: map[string]any{"body": payload}}})
	}
	return st
}
func BenchmarkScopedStateAgent(b *testing.B) {
	benchmarkScopedStateCase(b, auth.Principal{ID: "agent/a", Kind: "agent", Products: []string{"p"}, Permissions: []string{"read"}})
}
func BenchmarkScopedStateWildcard(b *testing.B) {
	benchmarkScopedStateCase(b, auth.Principal{ID: "agent/a", Kind: "agent", Products: []string{"*"}, Permissions: []string{"read"}})
}
func BenchmarkScopedStateAdmin(b *testing.B) {
	benchmarkScopedStateCase(b, auth.Principal{ID: "human/admin", Kind: "human", Role: "admin", Products: []string{"*"}, Permissions: []string{"admin"}})
}
func benchmarkScopedStateCase(b *testing.B, p auth.Principal) {
	st := benchmarkScopedState()
	ctx := auth.WithPrincipal(context.Background(), p)
	b.Run("typed", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := scopedState(ctx, st); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("json_reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := scopedStateJSONReference(ctx, st); err != nil {
				b.Fatal(err)
			}
		}
	})
}
