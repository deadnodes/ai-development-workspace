package application

import (
	"context"
	"maps"
	"releasecontrol/internal/domain"
	"slices"
	"time"
)

// Optional for test/external stores; persistent stores implement the borrow API.
func (s *Service) view(ctx context.Context, fn func(*domain.State) error) error {
	if v, ok := s.store.(interface {
		View(context.Context, func(*domain.State) error) error
	}); ok {
		return v.View(ctx, fn)
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	return fn(&st)
}
func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
func copyBuild(b domain.Build) domain.Build {
	b.Images = slices.Clone(b.Images)
	b.DispatchedAt = copyTime(b.DispatchedAt)
	b.StartedAt = copyTime(b.StartedAt)
	b.FinishedAt = copyTime(b.FinishedAt)
	return b
}
func copyDeployment(d domain.Deployment) domain.Deployment {
	d.RolloutDeadline = copyTime(d.RolloutDeadline)
	d.MergedAt = copyTime(d.MergedAt)
	d.Images = slices.Clone(d.Images)
	d.TargetFiles = slices.Clone(d.TargetFiles)
	d.CIContexts = maps.Clone(d.CIContexts)
	d.CheckedAt = copyTime(d.CheckedAt)
	d.RegistryCheckedAt = copyTime(d.RegistryCheckedAt)
	d.FinishedAt = copyTime(d.FinishedAt)
	return d
}

// Dispatchers retain only detached handles, never the borrowed full state, and
// release the Store before provider I/O or CAS updates.
func (s *Service) buildSnapshot(ctx context.Context) (domain.State, error) {
	var out domain.State
	err := s.view(ctx, func(st *domain.State) error {
		out.Builds = make([]domain.Build, len(st.Builds))
		for i, b := range st.Builds {
			out.Builds[i] = copyBuild(b)
		}
		return nil
	})
	return out, err
}
func (s *Service) deploymentSnapshot(ctx context.Context) (domain.State, error) {
	var out domain.State
	err := s.view(ctx, func(st *domain.State) error {
		out.Deployments = make([]domain.Deployment, len(st.Deployments))
		for i, d := range st.Deployments {
			out.Deployments[i] = copyDeployment(d)
		}
		return nil
	})
	return out, err
}
