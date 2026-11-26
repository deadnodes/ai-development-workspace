package application

import (
	"context"
	"errors"
	"testing"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

func userCtx(id, kind, owner, role string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{ID: id, Kind: kind, Owner: owner, Role: role, Products: []string{"*"}, Permissions: []string{"read", "write", "deploy", "admin"}})
}

func TestPrivateProductStateAndCommandScope(t *testing.T) {
	m := &memoryStore{domain.EmptyState()}
	s := New(m)
	owner := userCtx("alice", "human", "", "member")
	if _, err := s.Execute(owner, domain.Command{Action: "create_product", ID: "private-p", Data: map[string]any{"name": "Private", "visibility": "private"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(owner, domain.Command{Action: "create_feature", ID: "private-f", ProductID: "private-p", Data: map[string]any{"title": "Secret Feature", "goal": "secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(owner, domain.Command{Action: "create_product", ID: "shared-p", Data: map[string]any{"name": "Shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(owner, domain.Command{Action: "add_product_member", ProductID: "private-p", Data: map[string]any{"user_id": "bob"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		see  bool
	}{{"owner", owner, true}, {"member", userCtx("bob", "human", "", "member"), true}, {"agent-owner", userCtx("agent/a", "agent", "alice", "member"), true}, {"outsider", userCtx("charlie", "human", "", "member"), false}, {"agent-outsider", userCtx("agent/c", "agent", "charlie", "member"), false}, {"admin", userCtx("dana", "human", "", "admin"), false}, {"bootstrap", userCtx("service/bootstrap", "service", "", "admin"), true}, {"local", context.Background(), true}} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := s.State(tc.ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := false
			for _, p := range st.Products {
				if p.ID == "private-p" {
					got = true
				}
			}
			if got != tc.see {
				t.Fatalf("private visibility=%v want %v", got, tc.see)
			}
			for _, f := range st.Features {
				if f.ID == "private-f" && !tc.see {
					t.Fatal("private feature leaked")
				}
			}
			if len(st.Products) == 0 {
				t.Fatal("shared product hidden")
			}
		})
	}
	for _, caller := range []context.Context{userCtx("charlie", "human", "", "member"), userCtx("agent/c", "agent", "charlie", "member"), userCtx("dana", "human", "", "admin")} {
		_, err := s.Execute(caller, domain.Command{Action: "create_feature", ProductID: "private-p", Data: map[string]any{"title": "probe", "goal": "probe"}})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("write should look missing, got %v", err)
		}
	}
}

func TestPrivateProductLifecycleAndOwnerRules(t *testing.T) {
	m := &memoryStore{domain.EmptyState()}
	s := New(m)
	alice := userCtx("alice", "human", "", "member")
	bob := userCtx("bob", "human", "", "member")
	if _, err := s.Execute(alice, domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "P", "visibility": "private"}}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"add_product_member", "add_product_member"} {
		if _, err := s.Execute(alice, domain.Command{Action: action, ProductID: "p", Data: map[string]any{"user_id": "bob"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(m.state.Products[0].Members) != 1 {
		t.Fatalf("membership not idempotent: %#v", m.state.Products[0].Members)
	}
	if _, err := s.Execute(bob, domain.Command{Action: "remove_product_member", ProductID: "p", Data: map[string]any{"user_id": "alice"}}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("non-owner member removal: %v", err)
	}
	if _, err := s.Execute(alice, domain.Command{Action: "remove_product_member", ProductID: "p", Data: map[string]any{"user_id": "alice"}}); err == nil {
		t.Fatal("owner removal accepted")
	}
	if _, err := s.Execute(alice, domain.Command{Action: "set_product_visibility", ProductID: "p", Data: map[string]any{"visibility": "shared"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(bob, domain.Command{Action: "set_product_visibility", ProductID: "p", Data: map[string]any{"visibility": "private"}}); err == nil {
		t.Fatal("non-owner changed visibility")
	}
	if _, err := s.Execute(alice, domain.Command{Action: "set_product_visibility", ProductID: "p", Data: map[string]any{"visibility": "private"}}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateProductRequiresResolvableUser(t *testing.T) {
	s, _ := fixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{ID: "agent/no-owner", Kind: "agent", Products: []string{"*"}, Permissions: []string{"read", "write"}})
	_, err := s.Execute(ctx, domain.Command{Action: "create_product", Data: map[string]any{"name": "P", "visibility": "private"}})
	if !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("want forbidden, got %v", err)
	}
}

func TestSharedProductCanBeClaimedAndBootstrapCanManage(t *testing.T) {
	m := &memoryStore{domain.EmptyState()}
	s := New(m)
	if _, err := s.Execute(context.Background(), domain.Command{Action: "create_product", Actor: "setup", ID: "p", Data: map[string]any{"name": "Shared"}}); err != nil {
		t.Fatal(err)
	}
	alice := userCtx("alice", "human", "", "member")
	if _, err := s.Execute(alice, domain.Command{Action: "set_product_visibility", ProductID: "p", Data: map[string]any{"visibility": "private"}}); err != nil {
		t.Fatal(err)
	}
	if m.state.Products[0].Owner != "alice" {
		t.Fatalf("owner not assigned: %#v", m.state.Products[0])
	}
	bootstrap := userCtx("service/bootstrap", "service", "", "admin")
	if _, err := s.Execute(bootstrap, domain.Command{Action: "add_product_member", ProductID: "p", Data: map[string]any{"user_id": "bob"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Execute(bootstrap, domain.Command{Action: "set_product_visibility", ProductID: "p", Data: map[string]any{"visibility": "shared"}}); err != nil {
		t.Fatal(err)
	}
}
