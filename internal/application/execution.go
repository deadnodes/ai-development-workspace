package application

import (
	"context"
	"releasecontrol/internal/domain"
	"time"
)

func executionAction(a string) bool {
	switch a {
	case "claim_execution", "heartbeat_execution", "report_execution", "release_execution", "takeover_execution", "set_desired_state", "create_execution_group", "add_execution_group_member", "remove_execution_group_member", "set_product_stalled_timeout":
		return true
	}
	return false
}

func (s *Service) ExecutionReconcileInterval() time.Duration {
	seconds := 30
	if s.executionDefaults != nil {
		seconds = s.executionDefaults.ReconcileInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
func executionFor(st *domain.State, id string) *domain.IntegrationExecution {
	for i := range st.IntegrationExecutions {
		if st.IntegrationExecutions[i].IntegrationID == id {
			return &st.IntegrationExecutions[i]
		}
	}
	return nil
}
func executionFence(data map[string]any, key string) uint64 {
	switch v := data[key].(type) {
	case float64:
		if v >= 0 {
			return uint64(v)
		}
	case int:
		if v >= 0 {
			return uint64(v)
		}
	case int64:
		if v >= 0 {
			return uint64(v)
		}
	case uint64:
		return v
	}
	return 0
}
func executionCondition(st *domain.State, e *domain.IntegrationExecution, state string, required bool, reason string, now time.Time) {
	if e.State == state && e.Reconciliation.ActionRequired == required && e.Reconciliation.Reason == reason {
		return
	}
	e.State = state
	e.Reconciliation.ActionRequired = required
	e.Reconciliation.Reason = reason
	e.Reconciliation.Since = &now
	e.Reconciliation.Generation++
	st.Events = append(st.Events, domain.Event{ID: id(), Action: "execution_condition_changed", Actor: "system/execution-reconciler", At: now, EntityID: e.IntegrationID, ProductID: e.ProductID, Data: domain.Command{Action: "execution_condition_changed", Actor: "system/execution-reconciler", IntegrationID: e.IntegrationID, ProductID: e.ProductID, Data: map[string]any{"state": state, "action_required": required, "reason": reason, "generation": e.Reconciliation.Generation}}})
}
func (s *Service) executeExecutionCommand(ctx context.Context, c domain.Command) (any, error) {
	var out any
	err := s.store.Update(ctx, func(st *domain.State) error {
		if c.Actor == "" {
			return invalid("actor required")
		}
		now := time.Now().UTC()
		if c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in == nil {
				return missing("integration", c.IntegrationID)
			}
			if in.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
		}
		switch c.Action {
		case "claim_execution", "takeover_execution":
			in := integration(st, c.IntegrationID)
			if in == nil {
				return missing("integration", c.IntegrationID)
			}
			e := executionFor(st, c.IntegrationID)
			var x struct {
				OrchestratorID        string                  `json:"orchestrator_id"`
				AgentID               string                  `json:"agent_id"`
				Harness               domain.ExecutionHarness `json:"harness"`
				DesiredState          string                  `json:"desired_state"`
				StateCheckReference   string                  `json:"state_check_reference"`
				Fence                 uint64                  `json:"fence"`
				LeaseTTLSeconds       int                     `json:"lease_ttl_seconds"`
				StalledTimeoutSeconds *int                    `json:"stalled_timeout_seconds"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.DesiredState == "" {
				x.DesiredState = "ready"
			}
			if x.DesiredState != "ready" && x.DesiredState != "released" {
				return invalid("DESIRED_STATE_INVALID")
			}
			if x.LeaseTTLSeconds != 0 && (x.LeaseTTLSeconds < 30 || x.LeaseTTLSeconds > 86400) {
				return invalid("invalid lease ttl")
			}
			if x.StalledTimeoutSeconds != nil && (*x.StalledTimeoutSeconds < 0 || *x.StalledTimeoutSeconds > 604800) {
				return invalid("invalid stalled timeout")
			}
			if x.OrchestratorID == "" || x.AgentID == "" {
				return invalid("executor identity required")
			}
			if e != nil {
				if e.ProductID != in.ProductID || (c.ProductID != "" && c.ProductID != e.ProductID) {
					return invalid("SCOPE_MISMATCH")
				}
				if c.Action == "claim_execution" && e.State == "running" && e.LeaseUntil != nil && now.Before(*e.LeaseUntil) {
					return invalid("LEASE_HELD")
				}
				if c.Action == "claim_execution" && e.State != "unassigned" {
					return invalid("TAKEOVER_CHECK_REQUIRED")
				}
				if c.Action == "takeover_execution" {
					if x.StateCheckReference == "" {
						return invalid("TAKEOVER_CHECK_REQUIRED")
					}
					if x.Fence != e.Fence {
						return invalid("STALE_FENCE")
					}
					if e.State != "lost" && e.State != "stalled" && e.State != "exited_incomplete" && e.State != "unassigned" {
						return invalid("INVALID_STATE")
					}
				}
				e.Fence++
				e.State = "running"
				e.DesiredState = x.DesiredState
				e.OrchestratorID = x.OrchestratorID
				e.AgentID = x.AgentID
				e.Harness = x.Harness
				e.StalledTimeoutSeconds = x.StalledTimeoutSeconds
				executionCondition(st, e, "running", false, "", now)
				e.LeaseTTLSeconds = x.LeaseTTLSeconds
				if e.LeaseTTLSeconds <= 0 {
					e.LeaseTTLSeconds = 300
					if s.executionDefaults != nil {
						e.LeaseTTLSeconds = s.executionDefaults.LeaseTTL
					}
				}
				until := now.Add(time.Duration(e.LeaseTTLSeconds) * time.Second)
				e.LeaseUntil = &until
				e.ClaimedAt = &now
				e.LastHeartbeat = &now
				e.LastProgress = &now
			} else {
				if c.Action == "takeover_execution" {
					return missing("execution", c.IntegrationID)
				}
				if in.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				ttl := x.LeaseTTLSeconds
				if ttl <= 0 {
					ttl = 300
					if s.executionDefaults != nil {
						ttl = s.executionDefaults.LeaseTTL
					}
				}
				until := now.Add(time.Duration(ttl) * time.Second)
				e = &domain.IntegrationExecution{ID: id(), IntegrationID: in.ID, ProductID: in.ProductID, DesiredState: x.DesiredState, State: "running", OrchestratorID: x.OrchestratorID, AgentID: x.AgentID, Harness: x.Harness, Fence: 1, LeaseTTLSeconds: ttl, LeaseUntil: &until, ClaimedAt: &now, LastHeartbeat: &now, LastProgress: &now, Reconciliation: domain.ExecutionReconciliation{Enabled: true}}
				e.StalledTimeoutSeconds = x.StalledTimeoutSeconds
				e.Reconciliation.ActionRequired = false
				st.IntegrationExecutions = append(st.IntegrationExecutions, *e)
			}
			out = *e
		case "heartbeat_execution":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			if e.LeaseUntil == nil || !now.Before(*e.LeaseUntil) {
				return invalid("LEASE_EXPIRED")
			}
			if e.State != "running" && e.State != "stalled" {
				return invalid("INVALID_STATE")
			}
			nowp := now
			e.LastHeartbeat = &nowp
			until := now.Add(time.Duration(e.LeaseTTLSeconds) * time.Second)
			e.LeaseUntil = &until
			receipt, _ := c.Data["progress_receipt"].(string)
			if len(receipt) > 256 {
				return invalid("progress receipt exceeds limit")
			}
			if receipt != "" {
				e.LastProgress = &nowp
			}
			out = *e
		case "report_execution", "release_execution":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			state, _ := c.Data["state"].(string)
			if c.Action == "release_execution" {
				state, _ = c.Data["outcome"].(string)
				if state == "" {
					state = "exited_incomplete"
				}
			}
			if state != "waiting_external" && state != "blocked" && state != "paused" && state != "cancelled" && state != "exited_incomplete" && state != "completed" {
				return invalid("INVALID_STATE")
			}
			if state == "completed" {
				in := integration(st, c.IntegrationID)
				if in == nil || in.Status != e.DesiredState {
					return invalid("INVALID_STATE")
				}
			}
			required := state == "exited_incomplete"
			reason, _ := c.Data["reason"].(string)
			executionCondition(st, e, state, required, reason, now)
			e.ExitReason, _ = c.Data["reason"].(string)
			e.WaitingRef, _ = c.Data["waiting_ref"].(string)
			e.BlockerRef, _ = c.Data["blocker_ref"].(string)
			if state == "waiting_external" && e.WaitingRef == "" {
				return invalid("WAITING_REFERENCE_REQUIRED")
			}
			if state == "waiting_external" {
				found := false
				for _, op := range st.Operations {
					if op.ID == e.WaitingRef && op.ProductID == e.ProductID {
						found = true
					}
				}
				for _, b := range st.Builds {
					if b.ID == e.WaitingRef && b.ProductID == e.ProductID {
						found = true
					}
				}
				for _, d := range st.Deployments {
					if d.ID == e.WaitingRef && d.ProductID == e.ProductID {
						found = true
					}
				}
				if !found {
					return invalid("SCOPE_MISMATCH")
				}
			}
			if state == "blocked" && e.BlockerRef == "" {
				return invalid("BLOCKER_REFERENCE_REQUIRED")
			}
			if state == "blocked" {
				found := false
				for _, m := range st.Memories {
					if m.ID == e.BlockerRef && m.Kind == "blocker" {
						productID := m.ProductID
						if productID == "" {
							if f := feature(st, m.FeatureID); f != nil {
								productID = f.ProductID
							}
						}
						if productID == e.ProductID {
							found = true
						}
					}
				}
				if !found {
					return invalid("SCOPE_MISMATCH")
				}
			}
			out = *e
		case "set_desired_state":
			e := executionFor(st, c.IntegrationID)
			if e == nil {
				return missing("execution", c.IntegrationID)
			}
			if e.ProductID != c.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			if executionFence(c.Data, "fence") != e.Fence {
				return invalid("STALE_FENCE")
			}
			d, _ := c.Data["desired_state"].(string)
			if d != "ready" && d != "released" {
				return invalid("DESIRED_STATE_INVALID")
			}
			e.DesiredState = d
			out = *e
		case "create_execution_group":
			var x struct {
				Name           string   `json:"name"`
				OrchestratorID string   `json:"orchestrator_id"`
				IntegrationIDs []string `json:"integration_ids"`
			}
			if err := decode(c.Data, &x); err != nil {
				return err
			}
			if x.Name == "" || x.OrchestratorID == "" || len(x.IntegrationIDs) == 0 {
				return invalid("group fields required")
			}
			for _, iid := range x.IntegrationIDs {
				in := integration(st, iid)
				if in == nil {
					return missing("integration", iid)
				}
				if in.ProductID != c.ProductID {
					return invalid("SCOPE_MISMATCH")
				}
				if executionFor(st, iid) == nil {
					state := "unassigned"
					required := true
					reason := "no executor assigned"
					if in.Status == "ready" {
						state = "completed"
						required = false
						reason = ""
					}
					st.IntegrationExecutions = append(st.IntegrationExecutions, domain.IntegrationExecution{ID: id(), IntegrationID: iid, ProductID: in.ProductID, DesiredState: "ready", State: state, Reconciliation: domain.ExecutionReconciliation{Enabled: true, ActionRequired: required, Reason: reason, Since: &now}})
				}
			}
			g := domain.ExecutionGroup{ID: id(), ProductID: c.ProductID, Name: x.Name, OrchestratorID: x.OrchestratorID, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now, Revisions: []domain.ExecutionGroupRevision{{Revision: 1, MemberIntegrationIDs: x.IntegrationIDs, CreatedAt: now}}}
			st.ExecutionGroups = append(st.ExecutionGroups, g)
			out = g
		case "add_execution_group_member", "remove_execution_group_member":
			gid, _ := c.Data["group_id"].(string)
			iid, _ := c.Data["integration_id"].(string)
			var g *domain.ExecutionGroup
			for i := range st.ExecutionGroups {
				if st.ExecutionGroups[i].ID == gid {
					g = &st.ExecutionGroups[i]
				}
			}
			if g == nil {
				return missing("execution_group", gid)
			}
			if c.ProductID != g.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			in := integration(st, iid)
			if in == nil {
				return missing("integration", iid)
			}
			if in.ProductID != g.ProductID {
				return invalid("SCOPE_MISMATCH")
			}
			next := append([]string{}, g.MemberIntegrationIDs...)
			idx := -1
			for i, v := range next {
				if v == iid {
					idx = i
				}
			}
			if c.Action == "add_execution_group_member" && idx < 0 {
				next = append(next, iid)
			}
			if c.Action == "remove_execution_group_member" && idx >= 0 {
				next = append(next[:idx], next[idx+1:]...)
			}
			if (c.Action == "add_execution_group_member" && idx >= 0) || (c.Action == "remove_execution_group_member" && idx < 0) {
				out = *g
				return nil
			}
			g.MemberIntegrationIDs = next
			rev := uint64(len(g.Revisions) + 1)
			g.Revisions = append(g.Revisions, domain.ExecutionGroupRevision{Revision: rev, MemberIntegrationIDs: next, CreatedAt: now})
			out = *g
		case "set_product_stalled_timeout":
			var p *domain.Product
			for i := range st.Products {
				if st.Products[i].ID == c.ProductID {
					p = &st.Products[i]
					break
				}
			}
			if p == nil {
				return missing("product", c.ProductID)
			}
			if c.Data["clear"] == true {
				p.StalledTimeoutSeconds = nil
			} else {
				v, ok := c.Data["seconds"].(float64)
				if !ok || v < 0 || v > 604800 {
					return invalid("invalid stalled timeout")
				}
				n := int(v)
				p.StalledTimeoutSeconds = &n
			}
			out = *p
		}
		st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: now, EntityID: func() string {
			if c.IntegrationID != "" {
				return c.IntegrationID
			}
			if c.Action == "create_execution_group" {
				if g, ok := out.(domain.ExecutionGroup); ok {
					return g.ID
				}
			}
			return c.ProductID
		}(), ProductID: c.ProductID, Data: c})
		return nil
	})
	return out, err
}

func (s *Service) ReconcileExecutionsOnce(ctx context.Context) error {
	return s.store.Update(ctx, func(st *domain.State) error {
		now := time.Now().UTC()
		for i := range st.IntegrationExecutions {
			e := &st.IntegrationExecutions[i]
			if !e.Reconciliation.Enabled {
				continue
			}
			in := integration(st, e.IntegrationID)
			if in == nil {
				continue
			}
			next, required, reason := e.State, e.Reconciliation.ActionRequired, e.Reconciliation.Reason
			if in.Status == e.DesiredState {
				next, required, reason = "completed", false, ""
			} else {
				switch e.State {
				case "running":
					required, reason = false, ""
					if e.LeaseUntil != nil && now.After(*e.LeaseUntil) {
						next, required, reason = "lost", true, "lease expired"
					} else if e.LastProgress != nil {
						timeout := 900
						if s.executionDefaults != nil {
							timeout = s.executionDefaults.StalledTimeout
						}
						for j := range st.Products {
							if st.Products[j].ID == e.ProductID && st.Products[j].StalledTimeoutSeconds != nil {
								timeout = *st.Products[j].StalledTimeoutSeconds
							}
						}
						if e.StalledTimeoutSeconds != nil {
							timeout = *e.StalledTimeoutSeconds
						}
						if timeout > 0 {
							if now.Sub(*e.LastProgress) > time.Duration(timeout)*time.Second {
								next, required, reason = "stalled", true, "progress timeout"
							}
						}
					}
				case "stalled":
					timeout := 900
					if s.executionDefaults != nil {
						timeout = s.executionDefaults.StalledTimeout
					}
					for j := range st.Products {
						if st.Products[j].ID == e.ProductID && st.Products[j].StalledTimeoutSeconds != nil {
							timeout = *st.Products[j].StalledTimeoutSeconds
						}
					}
					if e.StalledTimeoutSeconds != nil {
						timeout = *e.StalledTimeoutSeconds
					}
					if timeout == 0 || (e.LastProgress != nil && now.Sub(*e.LastProgress) <= time.Duration(timeout)*time.Second) {
						next, required, reason = "running", false, ""
					}
				case "unassigned", "lost", "exited_incomplete":
					required = true
					if reason == "" {
						reason = next
					}
				case "paused", "cancelled", "completed":
					required = false
				case "waiting_external":
					required = false
					for _, op := range st.Operations {
						if op.ID == e.WaitingRef && op.Status != "PENDING" && op.Status != "RUNNING" {
							required = true
							reason = "external operation completed"
						}
					}
					for _, b := range st.Builds {
						if b.ID == e.WaitingRef && b.Status != "QUEUED" && b.Status != "RUNNING" && b.Status != "DISPATCHING" {
							required = true
							reason = "build operation completed"
						}
					}
					for _, d := range st.Deployments {
						if d.ID == e.WaitingRef && d.Status != "QUEUED" && d.Status != "RUNNING" && d.Status != "APPLYING" {
							required = true
							reason = "deployment operation completed"
						}
					}
				case "blocked":
					required = false
					for _, m := range st.Memories {
						if m.ID == e.BlockerRef && m.Kind == "blocker" && m.Status == "resolved" {
							required = true
							reason = "blocker resolved"
						}
					}
				}
			}
			executionCondition(st, e, next, required, reason, now)
		}
		return nil
	})
}
