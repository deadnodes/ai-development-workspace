package application

import (
	"context"
	"releasecontrol/internal/domain"
)

func (s *Service) InitializeWorkspaceProduct(ctx context.Context, name string) (domain.Product, error) {
	var product domain.Product
	update := s.store.Update
	if scoped, ok := s.store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	}); ok {
		update = func(ctx context.Context, change func(*domain.State) error) error {
			return scoped.UpdateScoped(ctx, domain.StateMutationScope{MutableCollections: []string{"products"}}, change)
		}
	}
	err := update(ctx, func(st *domain.State) error {
		if len(st.Products) != 0 {
			return nil
		}
		result, err := applyAuthorizedOrigin(ctx, st, domain.Command{Action: "create_product", Actor: "system/workspace", Data: map[string]any{"name": name}}, nil, nil)
		if err != nil {
			return err
		}
		product = result.(domain.Product)
		return nil
	})
	return product, err
}
