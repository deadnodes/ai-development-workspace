package application

import (
	"context"
	"maps"
	"releasecontrol/internal/auth"
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

func requireProductRead(ctx context.Context, st *domain.State, product string) error {
	if !auth.RequiresExistingProduct(ctx) {
		return nil
	}
	if record := productByID(*st, product); record == nil || !productVisibleTo(ctx, record) {
		return missing("product", product)
	}
	return nil
}

func (s *Service) requireProductAccess(ctx context.Context, product string) error {
	if !auth.RequiresExistingProduct(ctx) {
		return nil
	}
	if reader, ok := s.store.(interface {
		ReadProductAccess(context.Context, string) (bool, error)
	}); ok {
		visible, err := reader.ReadProductAccess(ctx, product)
		if err != nil {
			return err
		}
		if !visible {
			return missing("product", product)
		}
		return nil
	}
	return s.view(ctx, func(st *domain.State) error { return requireProductRead(ctx, st, product) })
}

func (s *Service) authorizeProductMutation(ctx context.Context, permission, product string) error {
	if err := s.requireProductAccess(ctx, product); err != nil {
		return err
	}
	return authorize(ctx, permission, product)
}

func authorizeScopedMutation(ctx context.Context, st *domain.State, permission, product string) error {
	if err := requireProductRead(ctx, st, product); err != nil {
		return err
	}
	return authorize(ctx, permission, product)
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
	b.Signatures = cloneSigning(b.Signatures)
	b.Security = slices.Clone(b.Security)
	for i := range b.Security {
		b.Security[i].BlockingIDs = slices.Clone(b.Security[i].BlockingIDs)
		b.Security[i].Reasons = slices.Clone(b.Security[i].Reasons)
	}
	b.CIContexts = maps.Clone(b.CIContexts)
	b.CIFailingContexts = maps.Clone(b.CIFailingContexts)
	b.DispatchedAt = copyTime(b.DispatchedAt)
	b.StartedAt = copyTime(b.StartedAt)
	b.FinishedAt = copyTime(b.FinishedAt)
	b.CICheckedAt = copyTime(b.CICheckedAt)
	b.CIFailedAt = copyTime(b.CIFailedAt)
	return b
}
func copyDeployment(d domain.Deployment) domain.Deployment {
	d.RolloutDeadline = copyTime(d.RolloutDeadline)
	d.MergedAt = copyTime(d.MergedAt)
	d.MainCICheckedAt = copyTime(d.MainCICheckedAt)
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
