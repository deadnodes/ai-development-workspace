package application

import (
	"context"
	"fmt"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"strings"
	"time"
)

func notAssignee(assignee string) error {
	return fmt.Errorf("%w: NOT_ASSIGNEE: assigned to %s; pick other work", auth.ErrForbidden, assignee)
}

// requireIntegrationUser applies execution ownership inside the same storage
// transaction as the protected mutation. Unassigned work is accepted only by
// callers that will assign it atomically (claim/group creation).
func requireIntegrationUser(ctx context.Context, in *domain.Integration, allowUnassigned bool) error {
	user, enforced := principalUser(ctx)
	if !enforced {
		return nil
	}
	if user == "" {
		return fmt.Errorf("%w: AGENT_OWNER_UNKNOWN", auth.ErrForbidden)
	}
	if in.Assignee != user && !(allowUnassigned && in.Assignee == "") {
		assignee := in.Assignee
		if assignee == "" {
			assignee = "unassigned"
		}
		return notAssignee(assignee)
	}
	return nil
}

// rejectForeignWork guards shared writes (progress, status) on an Integration:
// unassigned work stays open to everyone, including agents enrolled without an
// owner; work assigned to another user is refused.
func rejectForeignWork(ctx context.Context, in *domain.Integration) error {
	user, enforced := principalUser(ctx)
	if !enforced || in == nil || in.Assignee == "" || in.Assignee == user {
		return nil
	}
	return notAssignee(in.Assignee)
}

func executionVisibleTo(ctx context.Context, in *domain.Integration) bool {
	user, enforced := principalUser(ctx)
	if !enforced {
		return true
	}
	return user != "" && in != nil && (in.Assignee == "" || in.Assignee == user)
}

func executionOwnedByCaller(ctx context.Context, in *domain.Integration) bool {
	user, enforced := principalUser(ctx)
	if !enforced {
		return true
	}
	return user != "" && in != nil && in.Assignee == user
}

func (s *Service) executeAssignment(ctx context.Context, c domain.Command) (any, error) {
	if strings.TrimSpace(c.Actor) == "" {
		return nil, invalid("actor required")
	}
	for key := range c.Data {
		if c.Action == "assign_integration" && key != "assignee" && key != "force" || c.Action == "unassign_integration" {
			return nil, invalid("unknown field %s", key)
		}
	}
	if c.Action == "assign_integration" {
		if assignee, ok := c.Data["assignee"].(string); !ok || assignee == "" {
			return nil, invalid("assignee required")
		}
		if _, ok := c.Data["force"].(bool); c.Data["force"] != nil && !ok {
			return nil, invalid("force must be a boolean")
		}
	}
	var out any
	err := s.store.Update(ctx, func(st *domain.State) error {
		in := integration(st, c.IntegrationID)
		if in == nil {
			return missing("integration", c.IntegrationID)
		}
		if c.ProductID != "" && c.ProductID != in.ProductID {
			return invalid("SCOPE_MISMATCH")
		}
		user, enforced := principalUser(ctx)
		p, authenticated := auth.FromContext(ctx)
		admin := authenticated && instanceAdmin(p)
		if enforced && user == "" {
			return fmt.Errorf("%w: AGENT_OWNER_UNKNOWN", auth.ErrForbidden)
		}
		if enforced && !admin {
			if in.Assignee == "" {
				if c.Action == "unassign_integration" {
					return notAssignee("unassigned")
				}
			} else if in.Assignee != user {
				return notAssignee(in.Assignee)
			}
		}
		if c.Action == "assign_integration" {
			assignee, _ := c.Data["assignee"].(string)
			if assignee == "" {
				return invalid("assignee required")
			}
			if enforced && !admin && in.Assignee == "" && assignee != user {
				return notAssignee("unassigned")
			}
			force, _ := c.Data["force"].(bool)
			if force && !admin {
				return auth.ErrForbidden
			}
			for _, ex := range st.IntegrationExecutions {
				if assignee != in.Assignee && ex.IntegrationID == in.ID && ex.LeaseUntil != nil && time.Now().Before(*ex.LeaseUntil) && !(admin && force) {
					return fmt.Errorf("%w: ACTIVE_EXECUTION", ErrConflict)
				}
			}
			in.Assignee = assignee
		} else {
			in.Assignee = ""
		}
		now := time.Now().UTC()
		in.Actor, in.UpdatedAt = c.Actor, now
		out = *in
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: now, EntityID: in.ID, ProductID: in.ProductID, FeatureID: in.FeatureID, Data: c})
		return nil
	})
	return out, err
}
