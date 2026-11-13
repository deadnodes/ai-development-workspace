package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"releasecontrol/internal/domain"
)

type borrowedQueryStore struct{ state domain.State }

func (s *borrowedQueryStore) Read(context.Context) (domain.State, error) {
	return domain.State{}, errors.New("hot query used Read")
}
func (s *borrowedQueryStore) Update(_ context.Context, fn func(*domain.State) error) error {
	return fn(&s.state)
}
func (s *borrowedQueryStore) View(_ context.Context, fn func(*domain.State) error) error {
	return fn(&s.state)
}
func TestBuildDeploymentQueriesBorrowAndDetach(t *testing.T) {
	now := time.Now()
	store := &borrowedQueryStore{state: domain.EmptyState()}
	store.state.Builds = []domain.Build{{Meta: domain.Meta{ID: "b2", ProductID: "p"}, Images: []domain.BuildImage{{ID: "original"}}, FinishedAt: &now}, {Meta: domain.Meta{ID: "b1", ProductID: "p"}}}
	store.state.Deployments = []domain.Deployment{{Meta: domain.Meta{ID: "d2", ProductID: "p"}, CIContexts: map[string]string{"test": "original"}, TargetFiles: []string{"original"}, FinishedAt: &now}, {Meta: domain.Meta{ID: "d1", ProductID: "p"}}}
	s := New(store)
	ctx := context.Background()
	builds, err := s.ListBuilds(ctx, domain.ListBuildsRequest{ProductID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if builds.Items[0].ID != "b1" || store.state.Builds[0].ID != "b2" {
		t.Fatal("list changed cache order")
	}
	build, err := s.GetBuild(ctx, domain.BuildLookup{ProductID: "p", BuildID: "b2"})
	if err != nil {
		t.Fatal(err)
	}
	build.Images[0].ID = "mutated"
	*build.FinishedAt = time.Time{}
	deployments, err := s.ListDeployments(ctx, domain.ListDeploymentsRequest{ProductID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if deployments.Items[0].ID != "d1" || store.state.Deployments[0].ID != "d2" {
		t.Fatal("list changed cache order")
	}
	deployment, err := s.GetDeployment(ctx, domain.DeploymentLookup{ProductID: "p", DeploymentID: "d2"})
	if err != nil {
		t.Fatal(err)
	}
	deployment.CIContexts["test"] = "mutated"
	deployment.TargetFiles[0] = "mutated"
	if store.state.Builds[0].Images[0].ID != "original" || store.state.Builds[0].FinishedAt.IsZero() || store.state.Deployments[0].CIContexts["test"] != "original" || store.state.Deployments[0].TargetFiles[0] != "original" {
		t.Fatal("query result poisoned cache")
	}
}
