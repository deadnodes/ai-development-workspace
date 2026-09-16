package application

import (
	"context"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"testing"
)

type refFixture struct {
	fakeDelivery
	calls       int
	branch, sha string
}

func (f *refFixture) EnsureRef(_ context.Context, _ delivery.Connection, _, branch, sha string) (string, error) {
	f.calls++
	f.branch = branch
	f.sha = sha
	return sha, nil
}
func TestManagedBranchesAreAsyncScopedAndPinned(t *testing.T) {
	for _, action := range []string{"create_integration_branch", "protect_integration_revision"} {
		t.Run(action, func(t *testing.T) {
			_, m, _ := externalFixture(t)
			f := &refFixture{}
			s := NewWithProvider(m, f)
			in := integration(&m.state, "i")
			in.Repositories = []string{"source"}
			in.Branches = nil
			cmd := domain.Command{Action: action, ID: "managed", Actor: "agent", IntegrationID: "i", Data: map[string]any{"application_id": "component", "base_commit": shaBase, "revision_id": "rev", "approve": false}}
			if _, e := s.Execute(context.Background(), cmd); e == nil {
				t.Fatal("unapproved mutation accepted")
			}
			cmd.Data["approve"] = true
			if _, e := s.Execute(context.Background(), cmd); e != nil {
				t.Fatal(e)
			}
			if f.calls != 0 {
				t.Fatal("HTTP dispatch performed write")
			}
			tickNow(t, s, m)
			if m.state.Operations[0].Status != "SUCCEEDED" {
				t.Fatalf("%+v", m.state.Operations[0])
			}
			if f.calls != 1 {
				t.Fatal("missing ref write")
			}
			if action == "create_integration_branch" {
				if f.branch != "feature/rcp-i" || f.sha != shaBase || len(integration(&m.state, "i").Branches) != 1 {
					t.Fatal("incorrect branch binding")
				}
			} else if f.branch != "rcp/revisions/rev" || f.sha != shaHead {
				t.Fatal("incorrect pinned ref")
			}
		})
	}
}
func TestManagedBranchRejectsMovedBase(t *testing.T) {
	_, m, _ := externalFixture(t)
	f := &refFixture{}
	s := NewWithProvider(m, f)
	in := integration(&m.state, "i")
	in.Repositories = []string{"source"}
	in.Branches = nil
	exec(t, s, domain.Command{Action: "create_integration_branch", ID: "managed", IntegrationID: "i", Data: map[string]any{"application_id": "component", "base_commit": shaHead, "approve": true}})
	tickNow(t, s, m)
	if f.calls != 0 || m.state.Operations[0].Status != "FAILED" {
		t.Fatal("moved main accepted")
	}
}
