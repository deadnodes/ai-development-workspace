package application

import (
	"context"
	"fmt"
	"reflect"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"slices"
	"time"
)

func applyManagedBranch(st *domain.State, c domain.Command, m domain.Meta) (any, domain.Meta, error) {
	var in struct {
		ApplicationID string `json:"application_id"`
		BaseCommit    string `json:"base_commit"`
		RevisionID    string `json:"revision_id"`
		Approve       bool   `json:"approve"`
	}
	if err := decode(c.Data, &in); err != nil {
		return nil, m, err
	}
	integration := integration(st, c.IntegrationID)
	app := applicationByID(st, in.ApplicationID)
	if !in.Approve || integration == nil || integration.Status == "released" || app == nil || app.ProductID != integration.ProductID || !slices.Contains(integration.Repositories, app.RepositoryID) {
		return nil, m, invalid("explicit approval and integration-scoped component required")
	}
	binding := repositoryBinding(st, app.RepositoryID)
	if binding == nil || !domain.IsSourceRole(binding.Role) {
		return nil, m, invalid("attached source repository required")
	}
	conn, err := scopedConnection(st, binding.ConnectionID, integration.ProductID)
	if err != nil {
		return nil, m, err
	}
	for _, pending := range st.Operations {
		if pending.IntegrationID == integration.ID && pending.ApplicationID == app.ID && (pending.Kind == "CREATE_BRANCH" || pending.Kind == "PIN_REVISION") && !terminalOperation(pending.Status) {
			return nil, m, invalid("source ref operation already active")
		}
	}
	branch := "feature/rcp-" + integration.ID
	sha := in.BaseCommit
	kind := "CREATE_BRANCH"
	if c.Action == "protect_integration_revision" {
		rev := revisionByID(st, in.RevisionID)
		if rev == nil || rev.IntegrationID != integration.ID || rev.RepositoryID != app.RepositoryID {
			return nil, m, invalid("revision scope mismatch")
		}
		branch = "rcp/revisions/" + rev.ID
		sha = rev.HeadCommit
		kind = "PIN_REVISION"
	} else {
		for _, b := range integration.Branches {
			if b.RepositoryID == app.RepositoryID {
				return nil, m, invalid("integration already bound")
			}
		}
	}
	if !fullSHA.MatchString(sha) {
		return nil, m, invalid("exact source commit required")
	}
	op := domain.ExternalOperation{Meta: m, Kind: kind, IntegrationID: integration.ID, ApplicationID: app.ID, RequestedBy: c.Actor, Status: "PENDING", Phase: "CREATE_REF", DeploymentState: "PENDING", Sources: []domain.FlowSource{{RepositoryID: app.RepositoryID, Connection: conn.Config, Plan: delivery.SourcePlan{Repository: repositoryLocator(st, *binding), BaseRef: configuredBase(*binding), BaseSHA: sha, TargetBranch: branch, OperationID: m.ID}}}}
	st.Operations = append(st.Operations, op)
	return op, m, nil
}
func (s *Service) tickManagedBranch(ctx context.Context, op domain.ExternalOperation, token string) error {
	provider, ok := s.provider.(delivery.RefProvider)
	if !ok || len(op.Sources) != 1 {
		return s.finish(ctx, op, token, "BLOCKED", "provider cannot create managed refs", nil)
	}
	source := op.Sources[0]
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	binding := repositoryBinding(&st, source.RepositoryID)
	in := integration(&st, op.IntegrationID)
	if binding == nil || in == nil || in.Status == "released" || in.ProductID != op.ProductID || !slices.Contains(in.Repositories, source.RepositoryID) {
		return s.finish(ctx, op, token, "BLOCKED", "source scope changed", nil)
	}
	conn, err := scopedConnection(&st, binding.ConnectionID, op.ProductID)
	if err != nil || !reflect.DeepEqual(conn.Config, source.Connection) || repositoryLocator(&st, *binding) != source.Plan.Repository || configuredBase(*binding) != source.Plan.BaseRef {
		return s.finish(ctx, op, token, "BLOCKED", "connection changed", nil)
	}
	if op.Kind == "CREATE_BRANCH" {
		branches, e := s.provider.Branches(ctx, source.Connection, source.Plan.Repository)
		if e != nil {
			return s.finish(ctx, op, token, "FAILED", e.Error(), nil)
		}
		found := false
		for _, b := range branches {
			if b.Name == source.Plan.BaseRef && b.SHA == source.Plan.BaseSHA {
				found = true
			}
		}
		if !found {
			return s.finish(ctx, op, token, "BLOCKED", "base moved; request branch from current main commit", nil)
		}
	}
	sha, err := provider.EnsureRef(ctx, source.Connection, source.Plan.Repository, source.Plan.TargetBranch, source.Plan.BaseSHA)
	if err != nil {
		return s.finish(ctx, op, token, "FAILED", err.Error(), nil)
	}
	if sha != source.Plan.BaseSHA {
		return s.finish(ctx, op, token, "FAILED", "provider returned wrong source identity", nil)
	}
	if op.Kind == "CREATE_BRANCH" {
		err = s.store.Update(ctx, func(st *domain.State) error {
			current := operationByID(st, op.ID)
			if current == nil || current.LeaseOwner != token {
				return fmt.Errorf("operation lease lost")
			}
			in := integration(st, op.IntegrationID)
			if in == nil || in.ProductID != op.ProductID {
				return invalid("integration unavailable")
			}
			for _, b := range in.Branches {
				if b.RepositoryID == source.RepositoryID {
					if b.Name == source.Plan.TargetBranch {
						return nil
					}
					return invalid("integration binding changed")
				}
			}
			in.Branches = append(in.Branches, domain.Branch{RepositoryID: source.RepositoryID, Name: source.Plan.TargetBranch})
			in.UpdatedAt = time.Now().UTC()
			st.Events = append(st.Events, domain.Event{ID: id(), Action: "integration_branch_bound", Actor: op.RequestedBy, At: in.UpdatedAt, EntityID: in.ID, ProductID: in.ProductID, FeatureID: in.FeatureID})
			return nil
		})
		if err != nil {
			return s.finish(ctx, op, token, "BLOCKED", err.Error(), nil)
		}
	}
	return s.finish(ctx, op, token, "SUCCEEDED", "managed ref created and exact SHA verified; no history rewritten", map[string]string{"branch": source.Plan.TargetBranch, "source_sha": sha})
}
