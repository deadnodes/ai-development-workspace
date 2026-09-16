package application

import (
	"context"
	"path/filepath"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
	"strings"
)

type LocalGitRequest struct {
	ProductID    string `json:"product_id"`
	CheckoutID   string `json:"checkout_id"`
	Actor        string `json:"actor,omitempty"`
	Mode         string `json:"mode,omitempty"`
	ExpectedHEAD string `json:"expected_head,omitempty"`
	PlanID       string `json:"plan_id,omitempty"`
}
type LocalGitPlan struct {
	ID            string             `json:"id"`
	ProductID     string             `json:"product_id"`
	CheckoutID    string             `json:"checkout_id"`
	Mode          string             `json:"mode"`
	Status        string             `json:"status"`
	Executor      string             `json:"executor"`
	Observed      workspace.GitState `json:"observed"`
	GitArguments  []string           `json:"git_arguments"`
	Preconditions []string           `json:"preconditions"`
}

func (s *Service) localCheckout(ctx context.Context, product, checkout string) (domain.LocalCheckout, domain.Repository, error) {
	if s.workspaceRoot == "" {
		return domain.LocalCheckout{}, domain.Repository{}, invalid("server workspace not configured")
	}
	st, err := s.State(ctx)
	if err != nil {
		return domain.LocalCheckout{}, domain.Repository{}, err
	}
	root, err := filepath.Abs(s.workspaceRoot)
	if err != nil {
		return domain.LocalCheckout{}, domain.Repository{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return domain.LocalCheckout{}, domain.Repository{}, invalid("workspace unavailable")
	}
	for _, c := range st.LocalCheckouts {
		if c.ID == checkout && c.ProductID == product && c.Workspace == root {
			for _, r := range st.Repositories {
				if r.ID == c.RepositoryID && r.ProductID == product {
					return c, r, nil
				}
			}
		}
	}
	return domain.LocalCheckout{}, domain.Repository{}, missing("local checkout", checkout)
}
func (s *Service) LocalGitState(ctx context.Context, product, checkout string) (workspace.GitState, error) {
	c, r, err := s.localCheckout(ctx, product, checkout)
	if err != nil {
		return workspace.GitState{}, err
	}
	v, err := workspace.ObserveGit(ctx, s.workspaceRoot, c.RelativePath)
	if err != nil {
		return v, invalid("local Git observation: %s", err)
	}
	if v.Origin == "" || workspace.RemoteIdentity(v.Origin) == "" || workspace.RemoteIdentity(v.Origin) != workspace.RemoteIdentity(r.URL) {
		return v, invalid("origin no longer matches registered repository; rematch workspace")
	}
	return v, nil
}
func (s *Service) PlanLocalGitSync(ctx context.Context, in LocalGitRequest) (LocalGitPlan, error) {
	if strings.TrimSpace(in.Actor) == "" || (in.Mode != "FETCH" && in.Mode != "FAST_FORWARD") {
		return LocalGitPlan{}, invalid("actor and mode FETCH or FAST_FORWARD required")
	}
	v, err := s.LocalGitState(ctx, in.ProductID, in.CheckoutID)
	if err != nil {
		return LocalGitPlan{}, err
	}
	if in.ExpectedHEAD == "" || v.HEAD != in.ExpectedHEAD {
		return LocalGitPlan{}, invalid("expected_head must match current exact HEAD")
	}
	plan := LocalGitPlan{ID: id(), ProductID: in.ProductID, CheckoutID: in.CheckoutID, Mode: in.Mode, Status: "PLANNED", Executor: "EXTERNAL_AGENT", Observed: v, Preconditions: []string{"Resolve relative_path within your own trusted host workspace; server path may be a container mount.", "Re-read HEAD and require the recorded expected SHA before execution.", "Verify origin identity, Git configuration, credential helpers, hooks and filters are trusted before running local Git.", "No force, reset, rebase or push. The Control Plane does not execute these arguments.", "Record observed result using record_local_git_sync; a plan is not completion."}}
	plan.GitArguments = []string{"-c", "core.hooksPath=/dev/null", "fetch", "--no-tags", "--no-recurse-submodules", "origin"}
	if in.Mode == "FAST_FORWARD" {
		if v.SubmodulesPresent || v.Dirty || v.Branch == "" || v.UpstreamSHA == "" || v.Ahead > 0 || v.Behind == 0 {
			return LocalGitPlan{}, invalid("fast-forward requires a clean attached branch without submodules strictly behind origin upstream")
		}
		plan.GitArguments = []string{"-c", "core.hooksPath=/dev/null", "merge", "--ff-only", "--no-edit", v.UpstreamSHA}
		plan.Preconditions = append(plan.Preconditions, "Require the same branch, clean working tree and exact recorded upstream SHA; fetch first if remote freshness is needed. Do not run build/test hooks.")
	}
	err = s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, in.ProductID) {
			return missing("product", in.ProductID)
		}
		m := workspaceMeta(in.ProductID, in.Actor)
		m.ID = plan.ID
		workspaceAudit(st, m, "plan_local_git_sync", map[string]any{"plan": plan})
		return nil
	})
	return plan, err
}
func (s *Service) RecordLocalGitSync(ctx context.Context, in LocalGitRequest) (any, error) {
	if strings.TrimSpace(in.Actor) == "" || in.PlanID == "" {
		return nil, invalid("actor and plan_id required")
	}
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	var plan LocalGitPlan
	found := false
	for _, e := range st.Events {
		if e.ID != "" && e.EntityID == in.PlanID && e.ProductID == in.ProductID && e.Action == "plan_local_git_sync" {
			if err := decode(e.Data.Data, mapTarget(&plan)); err != nil {
				return nil, err
			}
			found = true
			break
		}
	}
	if !found || plan.CheckoutID != in.CheckoutID {
		return nil, missing("local sync plan", in.PlanID)
	}
	observed, err := s.LocalGitState(ctx, in.ProductID, in.CheckoutID)
	if err != nil {
		return nil, err
	}
	if plan.Mode == "FAST_FORWARD" && (observed.HEAD != plan.Observed.UpstreamSHA || observed.Branch != plan.Observed.Branch || observed.Dirty) {
		return nil, invalid("fast-forward result does not match planned clean branch and target SHA")
	}
	result := map[string]any{"plan_id": plan.ID, "status": "OBSERVED", "mode": plan.Mode, "state": observed, "note": "Local Git state re-read. FETCH network execution is agent-owned and cannot be proven from cached refs alone."}
	err = s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, in.ProductID) {
			return missing("product", in.ProductID)
		}
		m := workspaceMeta(in.ProductID, in.Actor)
		workspaceAudit(st, m, "record_local_git_sync", result)
		return nil
	})
	return result, err
}

// decode accepts the same persisted command data shape as normal application commands.
func mapTarget(plan *LocalGitPlan) any {
	return &struct {
		Plan *LocalGitPlan `json:"plan"`
	}{Plan: plan}
}
