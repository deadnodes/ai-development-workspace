package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"releasecontrol/internal/domain"
	"slices"
	"strings"
	"time"
)

var ErrValidation = errors.New("validation")
var ErrNotFound = errors.New("not found")

type Store interface {
	Read(context.Context) (domain.State, error)
	Update(context.Context, func(*domain.State) error) error
}
type Service struct{ store Store }

func New(store Store) *Service { return &Service{store: store} }
func (s *Service) State(ctx context.Context) (domain.State, error) {
	st, err := s.store.Read(ctx)
	if err == nil {
		derive(&st)
	}
	return st, err
}
func id() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func invalid(f string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(f, args...))
}
func missing(kind, id string) error { return fmt.Errorf("%w: %s %q", ErrNotFound, kind, id) }
func decode(data map[string]any, v any) error {
	b, e := json.Marshal(data)
	if e != nil {
		return invalid("invalid data")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(v); e != nil {
		return invalid("invalid fields: %v", e)
	}
	return nil
}
func (s *Service) Execute(ctx context.Context, c domain.Command) (any, error) {
	var result any
	err := s.store.Update(ctx, func(st *domain.State) error { var err error; result, err = apply(st, c); return err })
	return result, err
}
func meta(c domain.Command) domain.Meta {
	i := c.ID
	if i == "" {
		i = id()
	}
	n := time.Now().UTC()
	return domain.Meta{ID: i, ProductID: c.ProductID, FeatureID: c.FeatureID, Actor: c.Actor, CreatedAt: n, UpdatedAt: n}
}
func feature(st *domain.State, id string) *domain.Feature {
	for i := range st.Features {
		if st.Features[i].ID == id {
			return &st.Features[i]
		}
	}
	return nil
}
func integration(st *domain.State, id string) *domain.Integration {
	for i := range st.Integrations {
		if st.Integrations[i].ID == id {
			return &st.Integrations[i]
		}
	}
	return nil
}
func gate(st *domain.State, id string) *domain.Gate {
	for i := range st.Gates {
		if st.Gates[i].ID == id {
			return &st.Gates[i]
		}
	}
	return nil
}
func check(st *domain.State, id string) *domain.Check {
	for i := range st.Checks {
		if st.Checks[i].ID == id {
			return &st.Checks[i]
		}
	}
	return nil
}
func result(st *domain.State, id string) *domain.CheckResult {
	for i := range st.Results {
		if st.Results[i].ID == id {
			return &st.Results[i]
		}
	}
	return nil
}
func productExists(st *domain.State, id string) bool {
	for _, p := range st.Products {
		if p.ID == id {
			return true
		}
	}
	return false
}
func repositories(st *domain.State, ids []string, p string) error {
	for _, i := range ids {
		found := false
		for _, r := range st.Repositories {
			if r.ID == i && r.ProductID == p {
				found = true
			}
		}
		if !found {
			return invalid("repository %q is not in product", i)
		}
	}
	return nil
}
func env(st *domain.State, i, p string) error {
	if i == "" {
		return nil
	}
	for _, e := range st.Environments {
		if e.ID == i && e.ProductID == p {
			return nil
		}
	}
	return invalid("environment is not in product")
}
func refs(st *domain.State, ids []string, f string) error {
	for _, i := range ids {
		v := integration(st, i)
		if v == nil || v.FeatureID != f {
			return invalid("integration %q is not in feature", i)
		}
	}
	return nil
}
func latest(st *domain.State, c string) *domain.CheckResult {
	var r *domain.CheckResult
	for i := range st.Results {
		v := &st.Results[i]
		if v.CheckID == c && (r == nil || !v.CreatedAt.Before(r.CreatedAt)) {
			r = v
		}
	}
	return r
}
func gateResult(st *domain.State, g domain.Gate) string {
	count := 0
	for _, c := range st.Checks {
		if c.GateID != g.ID {
			continue
		}
		count++
		r := latest(st, c.ID)
		if r == nil {
			return "pending"
		}
		if r.Result != "passed" {
			return r.Result
		}
		if g.Commit != "" && r.Commit != g.Commit {
			return "stale"
		}
		if g.Deployment != "" && r.Deployment != g.Deployment {
			return "stale"
		}
		if g.EnvironmentID != "" && r.EnvironmentID != g.EnvironmentID {
			return "stale"
		}
	}
	if count == 0 {
		return "pending"
	}
	for _, f := range st.Findings {
		if f.Status == "open" && slices.Contains(f.GateIDs, g.ID) {
			return "blocked"
		}
	}
	return "passed"
}
func derive(st *domain.State) {
	for i := range st.Gates {
		st.Gates[i].Result = gateResult(st, st.Gates[i])
	}
}
func readiness(st *domain.State, in domain.Integration) error {
	for _, d := range in.Dependencies {
		v := integration(st, d)
		if v == nil || (v.Status != "ready" && v.Status != "released") {
			return invalid("dependency %s is not ready", d)
		}
	}
	for _, m := range st.Memories {
		if m.Kind == "blocker" && m.Status == "open" && m.FeatureID == in.FeatureID && (m.IntegrationID == "" || m.IntegrationID == in.ID) {
			return invalid("open blocker %s", m.ID)
		}
	}
	for _, f := range st.Findings {
		if f.Status == "open" && f.FeatureID == in.FeatureID && (slices.Contains(f.IntegrationIDs, in.ID) || (len(f.IntegrationIDs) == 0 && f.BlocksRelease)) {
			return invalid("open finding %s", f.ID)
		}
	}
	for _, g := range st.Gates {
		if g.FeatureID != in.FeatureID || !g.Blocking || !slices.Contains(g.IntegrationIDs, in.ID) {
			continue
		}
		if gateResult(st, g) != "passed" {
			return invalid("gate %s is not passing", g.ID)
		}
		if len(in.Commits) > 0 {
			for _, c := range st.Checks {
				if c.GateID != g.ID {
					continue
				}
				r := latest(st, c.ID)
				matched := false
				for _, commit := range in.Commits {
					if r != nil && r.Commit == commit.SHA {
						matched = true
					}
				}
				if !matched {
					return invalid("check %s evidence does not match integration commits", c.ID)
				}
			}
		}
	}
	return nil
}
func validateIntegration(st *domain.State, in domain.Integration) error {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Objective) == "" {
		return invalid("title and objective required")
	}
	if err := refs(st, in.Dependencies, in.FeatureID); err != nil {
		return err
	}
	if err := repositories(st, in.Repositories, in.ProductID); err != nil {
		return err
	}
	for _, b := range in.Branches {
		if err := repositories(st, []string{b.RepositoryID}, in.ProductID); err != nil {
			return err
		}
	}
	for _, c := range in.Commits {
		if c.SHA == "" {
			return invalid("commit sha required")
		}
		if err := repositories(st, []string{c.RepositoryID}, in.ProductID); err != nil {
			return err
		}
	}
	for _, pr := range in.PullRequests {
		if err := repositories(st, []string{pr.RepositoryID}, in.ProductID); err != nil {
			return err
		}
	}
	var visit func(string, map[string]bool) bool
	visit = func(i string, seen map[string]bool) bool {
		if i == in.ID {
			return true
		}
		if seen[i] {
			return false
		}
		seen[i] = true
		v := integration(st, i)
		if v != nil {
			for _, d := range v.Dependencies {
				if visit(d, seen) {
					return true
				}
			}
		}
		return false
	}
	for _, d := range in.Dependencies {
		if visit(d, map[string]bool{}) {
			return invalid("dependency cycle")
		}
	}
	return nil
}
func apply(st *domain.State, c domain.Command) (any, error) {
	if strings.TrimSpace(c.Actor) == "" {
		return nil, invalid("actor required")
	}
	if !slices.Contains(domain.Actions, c.Action) {
		return nil, invalid("unknown action")
	}
	for _, key := range []string{"id", "product_id", "feature_id", "integration_id", "gate_id", "check_id", "result_id", "actor", "created_at", "updated_at", "resolution", "fix_commit", "desired_composition_id"} {
		if _, exists := c.Data[key]; exists {
			return nil, invalid("field %s cannot be supplied in data", key)
		}
	}
	for key := range c.Data {
		if key != strings.ToLower(key) {
			return nil, invalid("data field %s must use snake_case", key)
		}
	}
	m := meta(c)
	var out any
	if c.IntegrationID != "" {
		v := integration(st, c.IntegrationID)
		if v == nil {
			return nil, missing("integration", c.IntegrationID)
		}
		if c.FeatureID != "" && c.FeatureID != v.FeatureID {
			return nil, invalid("integration feature mismatch")
		}
		c.FeatureID = v.FeatureID
	}
	if c.FeatureID != "" {
		f := feature(st, c.FeatureID)
		if f == nil {
			return nil, missing("feature", c.FeatureID)
		}
		if c.ProductID != "" && c.ProductID != f.ProductID {
			return nil, invalid("feature product mismatch")
		}
		c.ProductID = f.ProductID
	}
	m.ProductID = c.ProductID
	m.FeatureID = c.FeatureID
	if c.ID != "" && !slices.Contains([]string{"update_feature", "update_integration", "start_integration", "complete_integration", "transition_integration", "resolve_blocker", "resolve_finding", "update_environment", "select_composition"}, c.Action) {
		b, _ := json.Marshal(st)
		var arrays map[string][]map[string]any
		_ = json.Unmarshal(b, &arrays)
		for _, a := range arrays {
			for _, v := range a {
				if v["id"] == c.ID {
					return nil, invalid("id already exists")
				}
			}
		}
	}
	switch c.Action {
	case "create_application", "record_integration_revision", "plan_composition", "select_composition":
		var err error
		out, m, err = applyComposition(st, c, m)
		if err != nil {
			return nil, err
		}
	case "create_product":
		v := domain.Product{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		if strings.TrimSpace(v.Name) == "" {
			return nil, invalid("name required")
		}
		st.Products = append(st.Products, v)
		out = v
	case "create_feature", "update_feature":
		var v domain.Feature
		var old *domain.Feature
		if c.Action == "update_feature" {
			old = feature(st, c.FeatureID)
			if old == nil {
				return nil, missing("feature", c.FeatureID)
			}
			v = *old
			m = old.Meta
			m.Actor = c.Actor
			m.UpdatedAt = time.Now().UTC()
		} else {
			v.Status = "active"
			if !productExists(st, c.ProductID) {
				return nil, missing("product", c.ProductID)
			}
		}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		v.FeatureID = ""
		if strings.TrimSpace(v.Title) == "" || strings.TrimSpace(v.Goal) == "" {
			return nil, invalid("title and goal required")
		}
		if e := repositories(st, v.Repositories, v.ProductID); e != nil {
			return nil, e
		}
		if old != nil {
			*old = v
		} else {
			st.Features = append(st.Features, v)
		}
		out = v
	case "create_integration", "update_integration":
		var v domain.Integration
		var old *domain.Integration
		if c.Action == "update_integration" {
			old = integration(st, c.IntegrationID)
			if old == nil {
				return nil, missing("integration", c.IntegrationID)
			}
			if old.Status == "released" {
				return nil, invalid("released integration is immutable")
			}
			v = *old
			m = old.Meta
			m.Actor = c.Actor
			m.UpdatedAt = time.Now().UTC()
			if _, ok := c.Data["status"]; ok {
				return nil, invalid("use transition_integration for status")
			}
		} else {
			if feature(st, c.FeatureID) == nil {
				return nil, missing("feature", c.FeatureID)
			}
			v.Status = "planned"
		}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		if old == nil && v.Status != "planned" {
			return nil, invalid("new integrations must be planned")
		}
		if e := validateIntegration(st, v); e != nil {
			return nil, e
		}
		if old != nil && v.Status == "ready" {
			v.Status = "working"
		}
		if old != nil {
			*old = v
		} else {
			st.Integrations = append(st.Integrations, v)
		}
		out = v
	case "start_integration", "complete_integration", "transition_integration":
		v := integration(st, c.IntegrationID)
		if v == nil {
			return nil, missing("integration", c.IntegrationID)
		}
		if v.Status == "released" {
			return nil, invalid("released integration is immutable")
		}
		status, _ := c.Data["status"].(string)
		if c.Action == "start_integration" {
			status = "working"
		}
		if c.Action == "complete_integration" {
			status = "ready"
		}
		if !slices.Contains([]string{"planned", "working", "implemented", "verifying", "ready"}, status) {
			return nil, invalid("unsupported status")
		}
		if status == "ready" {
			if e := readiness(st, *v); e != nil {
				return nil, e
			}
		}
		v.Status = status
		if status == "working" {
			v.Owner = c.Actor
		}
		v.Actor = c.Actor
		v.UpdatedAt = m.UpdatedAt
		out = *v
		m = v.Meta
	case "record_progress", "record_decision", "record_discovery", "add_blocker", "handoff":
		if feature(st, c.FeatureID) == nil {
			return nil, missing("feature", c.FeatureID)
		}
		v := domain.Memory{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		v.IntegrationID = c.IntegrationID
		v.Kind = map[string]string{"record_progress": "progress", "record_decision": "decision", "record_discovery": "discovery", "add_blocker": "blocker", "handoff": "handoff"}[c.Action]
		v.Status = ""
		v.Resolution = ""
		if v.Kind == "blocker" {
			v.Status = "open"
		}
		if v.Title == "" && v.Body == "" && v.Current == "" && len(v.Completed) == 0 && len(v.Next) == 0 {
			return nil, invalid("memory content required")
		}
		if v.Kind == "decision" && v.Reason == "" {
			return nil, invalid("decision reason required")
		}
		if c.Action == "record_progress" && c.IntegrationID != "" {
			in := integration(st, c.IntegrationID)
			if in.Status == "released" {
				return nil, invalid("released integration is immutable")
			}
			if _, ok := c.Data["completed"]; ok {
				in.Completed = v.Completed
			}
			if _, ok := c.Data["remaining"]; ok {
				in.Remaining = v.Remaining
			}
			in.UpdatedAt = m.UpdatedAt
		}
		st.Memories = append(st.Memories, v)
		out = v
	case "resolve_blocker":
		found := false
		for i := range st.Memories {
			v := &st.Memories[i]
			if v.ID != c.ID || v.Kind != "blocker" {
				continue
			}
			if v.Status != "open" {
				return nil, invalid("blocker already resolved")
			}
			body, _ := c.Data["body"].(string)
			if body == "" {
				return nil, invalid("resolution body required")
			}
			v.Status = "resolved"
			v.Resolution = body
			v.UpdatedAt = m.UpdatedAt
			m = v.Meta
			out = *v
			found = true
		}
		if !found {
			return nil, missing("blocker", c.ID)
		}
	case "create_gate":
		if feature(st, c.FeatureID) == nil {
			return nil, missing("feature", c.FeatureID)
		}
		v := domain.Gate{Blocking: true}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		v.Result = "pending"
		if v.Title == "" || v.Reason == "" || len(v.IntegrationIDs) == 0 {
			return nil, invalid("title, reason and integration_ids required")
		}
		if e := refs(st, v.IntegrationIDs, v.FeatureID); e != nil {
			return nil, e
		}
		if e := env(st, v.EnvironmentID, v.ProductID); e != nil {
			return nil, e
		}
		st.Gates = append(st.Gates, v)
		out = v
	case "add_check":
		g := gate(st, c.GateID)
		if g == nil {
			return nil, missing("gate", c.GateID)
		}
		v := domain.Check{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		m.ProductID = g.ProductID
		m.FeatureID = g.FeatureID
		v.Meta = m
		v.GateID = g.ID
		if v.Title == "" || v.Mechanism == "" {
			return nil, invalid("title and mechanism required")
		}
		st.Checks = append(st.Checks, v)
		out = v
	case "record_check_result":
		ch := check(st, c.CheckID)
		if ch == nil {
			return nil, missing("check", c.CheckID)
		}
		v := domain.CheckResult{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		m.ProductID = ch.ProductID
		m.FeatureID = ch.FeatureID
		v.Meta = m
		v.CheckID = ch.ID
		if v.Commit == "" && v.Deployment == "" {
			return nil, invalid("tested commit or deployment required")
		}
		if !slices.Contains([]string{"passed", "failed", "blocked", "skipped"}, v.Result) {
			return nil, invalid("invalid check result")
		}
		if e := env(st, v.EnvironmentID, v.ProductID); e != nil {
			return nil, e
		}
		st.Results = append(st.Results, v)
		out = v
	case "record_finding":
		r := result(st, c.ResultID)
		if r == nil {
			return nil, missing("result", c.ResultID)
		}
		if r.Result != "failed" && r.Result != "blocked" {
			return nil, invalid("findings require failed or blocked result")
		}
		v := domain.Finding{BlocksRelease: true}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		m.ProductID = r.ProductID
		m.FeatureID = r.FeatureID
		v.Meta = m
		v.ResultID = r.ID
		v.Status = "open"
		v.Resolution = ""
		v.FixCommit = ""
		if v.Title == "" || v.Severity == "" {
			return nil, invalid("title and severity required")
		}
		if e := refs(st, v.IntegrationIDs, v.FeatureID); e != nil {
			return nil, e
		}
		if len(v.GateIDs) == 0 {
			v.GateIDs = []string{check(st, r.CheckID).GateID}
		}
		for _, i := range v.GateIDs {
			g := gate(st, i)
			if g == nil || g.FeatureID != v.FeatureID {
				return nil, invalid("finding gate outside feature")
			}
		}
		st.Findings = append(st.Findings, v)
		out = v
	case "resolve_finding":
		found := false
		for i := range st.Findings {
			v := &st.Findings[i]
			if v.ID != c.ID {
				continue
			}
			if v.Status != "open" {
				return nil, invalid("finding already resolved")
			}
			body, _ := c.Data["body"].(string)
			if body == "" {
				return nil, invalid("resolution body required")
			}
			v.Status = "resolved"
			v.Resolution = body
			v.FixCommit, _ = c.Data["commit"].(string)
			v.UpdatedAt = m.UpdatedAt
			m = v.Meta
			out = *v
			found = true
		}
		if !found {
			return nil, missing("finding", c.ID)
		}
	case "create_environment", "update_environment":
		var v domain.Environment
		var old *domain.Environment
		if c.Action == "update_environment" {
			for i := range st.Environments {
				if st.Environments[i].ID == c.ID {
					old = &st.Environments[i]
				}
			}
			if old == nil {
				return nil, missing("environment", c.ID)
			}
			v = *old
			m = old.Meta
			m.Actor = c.Actor
			m.UpdatedAt = time.Now().UTC()
		} else if !productExists(st, c.ProductID) {
			return nil, missing("product", c.ProductID)
		}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		v.Name = strings.TrimSpace(v.Name)
		v.Cluster = strings.TrimSpace(v.Cluster)
		v.Namespace = strings.TrimSpace(v.Namespace)
		if v.Name == "" {
			return nil, invalid("name required")
		}
		for _, existing := range st.Environments {
			if existing.ID != v.ID && existing.ProductID == v.ProductID && strings.EqualFold(strings.TrimSpace(existing.Name), v.Name) {
				return nil, invalid("environment name already exists in product")
			}
		}
		if old != nil && (old.Cluster != v.Cluster || old.Namespace != v.Namespace) {
			v.DesiredCompositionID = ""
		}
		if old != nil {
			*old = v
		} else {
			st.Environments = append(st.Environments, v)
		}
		out = v
	case "create_repository":
		if !productExists(st, c.ProductID) {
			return nil, missing("product", c.ProductID)
		}
		v := domain.Repository{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		if v.Name == "" || v.URL == "" {
			return nil, invalid("name and url required")
		}
		st.Repositories = append(st.Repositories, v)
		out = v
	case "plan_release":
		if !productExists(st, c.ProductID) {
			return nil, missing("product", c.ProductID)
		}
		v := domain.Release{}
		if e := decode(c.Data, &v); e != nil {
			return nil, e
		}
		v.Meta = m
		v.Status = "planned"
		v.Snapshots = []domain.Integration{}
		if v.Name == "" || len(v.IntegrationIDs) == 0 {
			return nil, invalid("name and integration_ids required")
		}
		for _, i := range v.IntegrationIDs {
			in := integration(st, i)
			if in == nil || in.ProductID != v.ProductID {
				return nil, invalid("integration outside product")
			}
			if in.Status != "ready" && in.Status != "released" {
				return nil, invalid("release integration must be ready")
			}
			if e := readiness(st, *in); e != nil {
				return nil, e
			}
			v.Snapshots = append(v.Snapshots, *in)
			for _, dep := range in.Dependencies {
				d := integration(st, dep)
				if d.Status != "released" && !slices.Contains(v.IntegrationIDs, dep) {
					return nil, invalid("release excludes unreleased dependency %s", dep)
				}
			}
		}
		for _, i := range v.ExcludedIntegrationIDs {
			in := integration(st, i)
			if in == nil || in.ProductID != v.ProductID || slices.Contains(v.IntegrationIDs, i) {
				return nil, invalid("invalid excluded integration")
			}
		}
		st.Releases = append(st.Releases, v)
		out = v
	}
	if c.Action == "create_feature" || c.Action == "update_feature" {
		m.FeatureID = m.ID
	}
	st.Events = append(st.Events, domain.Event{ID: id(), Action: c.Action, Actor: c.Actor, At: time.Now().UTC(), EntityID: m.ID, ProductID: m.ProductID, FeatureID: m.FeatureID, Data: c})
	derive(st)
	return out, nil
}
