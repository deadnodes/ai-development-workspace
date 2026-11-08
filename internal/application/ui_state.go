package application

import (
	"context"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"slices"
)

type uiStateReader interface {
	ReadUIState(context.Context, domain.UIStateRequest) (domain.UIState, error)
}

func (s *Service) UIState(ctx context.Context, product, feature string) (domain.UIState, error) {
	if err := authorize(ctx, "read", product); err != nil {
		return domain.UIState{}, err
	}
	if (product != "" && !stateID(product)) || (feature != "" && !stateID(feature)) {
		return domain.UIState{}, invalid("invalid UI scope")
	}
	q := domain.UIStateRequest{ProductID: product, FeatureID: feature}
	if p, ok := auth.FromContext(ctx); ok && !instanceAdmin(p) && !slices.Contains(p.Products, "*") {
		q.RestrictProducts = true
		q.AllowedProductIDs = append([]string{}, p.Products...)
	}
	reader, ok := s.store.(uiStateReader)
	if !ok {
		return domain.UIState{}, invalid("bounded UI reader unavailable")
	}
	out, err := reader.ReadUIState(ctx, q)
	if err != nil {
		return out, err
	}
	out.State, err = scopedState(ctx, out.State)
	if err != nil {
		return domain.UIState{}, err
	}
	derive(&out.State)
	return out, nil
}
