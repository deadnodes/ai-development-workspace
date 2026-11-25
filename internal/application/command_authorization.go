package application

import (
	"context"
	"errors"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
)

// Specialized commands may perform several reads and transactions around
// provider IO. Authorize every admission and each atomic mutation against its
// own current state, using the immutable, attributed incoming command.
type commandAuthorizationStore struct {
	Store
	command               domain.Command
	checkCreationIdentity bool
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
	update := s.Store.Update
	// These command implementations mutate only these collections (including
	// derived gates). Other commands retain the fully isolated callback contract.
	var scope *domain.StateMutationScope
	switch s.command.Action {
	case "claim_execution": // a claim also assigns an unassigned Integration to the caller's user
		scope = &domain.StateMutationScope{MutableCollections: []string{"integrations", "integration_executions", "execution_events"}, AppendCollections: []string{"events"}}
	case "takeover_execution", "heartbeat_execution", "report_execution", "release_execution", "set_desired_state":
		scope = &domain.StateMutationScope{MutableCollections: []string{"integration_executions", "execution_events"}, AppendCollections: []string{"events"}}
	case "assign_integration", "unassign_integration":
		scope = &domain.StateMutationScope{MutableCollections: []string{"integrations"}, AppendCollections: []string{"events"}}
	case "report_turn_end":
		scope = &domain.StateMutationScope{MutableCollections: []string{"integration_executions"}, AppendCollections: []string{"events"}}
	case "claim_execution_event_delivery", "ack_execution_event", "resolve_execution_event":
		scope = &domain.StateMutationScope{MutableCollections: []string{"execution_events"}, AppendCollections: []string{"events"}}
	case "create_execution_group":
		scope = &domain.StateMutationScope{MutableCollections: []string{"integrations"}, AppendCollections: []string{"integration_executions", "execution_groups", "events", "execution_events"}}
	case "add_execution_group_member", "remove_execution_group_member":
		scope = &domain.StateMutationScope{MutableCollections: []string{"execution_groups"}, AppendCollections: []string{"events"}}
	case "set_execution_group_harness":
		scope = &domain.StateMutationScope{MutableCollections: []string{"execution_groups"}, AppendCollections: []string{"events"}}
	case "set_product_stalled_timeout":
		scope = &domain.StateMutationScope{MutableCollections: []string{"products"}, AppendCollections: []string{"events"}}
	case "record_runtime_observation":
		scope = &domain.StateMutationScope{MutableCollections: []string{"gates"}, AppendCollections: []string{"runtime_observations", "events"}}
	case "record_progress":
		if _, fenced := s.command.Data["execution_fence"]; fenced {
			scope = &domain.StateMutationScope{MutableCollections: []string{"integrations", "integration_executions", "execution_events", "gates"}, AppendCollections: []string{"memories", "events"}}
		} else {
			scope = &domain.StateMutationScope{MutableCollections: []string{"integrations", "integration_executions", "gates"}, AppendCollections: []string{"memories"}}
		}
	case "record_decision", "record_discovery", "add_blocker", "handoff":
		scope = &domain.StateMutationScope{MutableCollections: []string{"gates"}, AppendCollections: []string{"memories"}}
	case "create_integration", "update_integration", "start_integration", "complete_integration", "transition_integration":
		// Creating or reopening an integration can reactivate its parent.
		if _, fenced := s.command.Data["execution_fence"]; fenced {
			scope = &domain.StateMutationScope{MutableCollections: []string{"features", "integrations", "gates", "integration_executions", "execution_events"}, AppendCollections: []string{"events"}}
		} else {
			scope = &domain.StateMutationScope{MutableCollections: []string{"features", "integrations", "gates"}}
		}
	case "create_feature", "update_feature":
		scope = &domain.StateMutationScope{MutableCollections: []string{"features", "gates"}}
	case "resolve_blocker":
		scope = &domain.StateMutationScope{MutableCollections: []string{"memories", "gates"}}
	}

	// Creation identity checks include journal IDs outside the write projection.
	// Delivery retries have their own exact replay admission and keep that path.
	if s.checkCreationIdentity && commandNeedsNewID(s.command) {
		if scope == nil {
			scope = &domain.StateMutationScope{AllCollections: true}
		}
		scope.RequireAbsentIDs = []string{s.command.ID}
		scope.AllowExistingRecords = s.command.Action == "deploy_integration" || s.command.Action == "deploy_existing_artifact" || s.command.Action == "update_environment_feature"
	}

	if _, authenticated := auth.FromContext(ctx); authenticated {
		ids, err := commandReferenceIDs(s.command)
		if err != nil {
			return err
		}
		if scope == nil {
			scope = &domain.StateMutationScope{AllCollections: true}
		}
		scope.EventReferences = ids
	}
	if scoped, ok := s.Store.(interface {
		UpdateScoped(context.Context, domain.StateMutationScope, func(*domain.State) error) error
	}); ok && scope != nil {
		update = func(ctx context.Context, fn func(*domain.State) error) error {
			return scoped.UpdateScoped(ctx, *scope, fn)
		}
	}
	err := update(ctx, func(st *domain.State) error {
		if _, authenticated := auth.FromContext(ctx); authenticated {
			if err := authorizeCommand(ctx, *st, s.command); err != nil {
				return err
			}
		}
		return change(st)
	})
	if errors.Is(err, domain.ErrIdentityExists) {
		return invalid("id already exists")
	}
	return err
}
