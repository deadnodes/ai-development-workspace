package application

import (
	"context"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

// Specialized commands may perform several reads and transactions around
// provider IO. Authorize every admission and each atomic mutation against its
// own current state, using the immutable, attributed incoming command.
type commandAuthorizationStore struct {
	Store
	command domain.Command
}

func (s commandAuthorizationStore) Read(ctx context.Context) (domain.State, error) {
	st, err := s.Store.Read(ctx)
	if err != nil {
		return domain.State{}, err
	}
	if err = authorizeCommand(ctx, st, s.command); err != nil {
		return domain.State{}, err
	}
	return st, nil
}

func (s commandAuthorizationStore) Update(ctx context.Context, change func(*domain.State) error) error {
	return s.Store.Update(ctx, func(st *domain.State) error {
		if _, authenticated := auth.FromContext(ctx); authenticated {
			if err := authorizeCommand(ctx, *st, s.command); err != nil {
				return err
			}
		}
		return change(st)
	})
}
