package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"releasecontrol/internal/domain"
	"releasecontrol/internal/workspace"
)

func (s *Service) SetWorkspaceRoot(root string) { s.workspaceRoot = root }
func workspaceMeta(product, actor string) domain.Meta {
	now := time.Now().UTC()
	return domain.Meta{ID: id(), ProductID: product, Actor: actor, CreatedAt: now, UpdatedAt: now}
}
func workspaceAudit(st *domain.State, m domain.Meta, action string, data map[string]any) {
	st.Events = append(st.Events, domain.Event{ID: id(), Action: action, Actor: m.Actor, At: m.UpdatedAt, EntityID: m.ID, ProductID: m.ProductID, Data: domain.Command{Action: action, Actor: m.Actor, ProductID: m.ProductID, Data: data}})
}
func knowledgeFor(st domain.State, product string) domain.ProjectKnowledge {
	for n := len(st.ProjectKnowledge) - 1; n >= 0; n-- {
		if st.ProjectKnowledge[n].ProductID == product {
			return st.ProjectKnowledge[n]
		}
	}
	return domain.ProjectKnowledge{Areas: []domain.ProductArea{}, Relationships: []domain.AreaRelationship{}, Parameters: map[string]string{}}
}
func validateKnowledge(st *domain.State, product string, k domain.ProjectKnowledge) error {
	areas := map[string]domain.ProductArea{}
	for _, a := range k.Areas {
		if a.ID == "" || a.Name == "" {
			return invalid("area id and name required")
		}
		if _, ok := areas[a.ID]; ok {
			return invalid("duplicate area")
		}
		areas[a.ID] = a
		if err := repositories(st, a.RepositoryIDs, product); err != nil {
			return err
		}
	}
	for _, a := range k.Areas {
		seen := map[string]bool{a.ID: true}
		p := a.ParentID
		for p != "" {
			v, ok := areas[p]
			if !ok || seen[p] {
				return invalid("invalid area parent or cycle")
			}
			seen[p] = true
			p = v.ParentID
		}
	}
	for _, r := range k.Relationships {
		_, a := areas[r.From]
		_, b := areas[r.To]
		if !a || !b || r.From == r.To || !slices.Contains([]string{"DEPENDS_ON", "CONSUMES", "PROVIDES_TO", "SHARES_DATA_WITH"}, r.Type) {
			return invalid("invalid area relationship")
		}
	}
	return nil
}
func (s *Service) SetProjectKnowledge(ctx context.Context, product, actor string, k domain.ProjectKnowledge) (any, error) {
	if strings.TrimSpace(actor) == "" {
		return nil, invalid("actor required")
	}
	err := s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, product) {
			return missing("product", product)
		}
		if err := validateKnowledge(st, product, k); err != nil {
			return err
		}
		k.Meta = workspaceMeta(product, actor)
		st.ProjectKnowledge = append(st.ProjectKnowledge, k)
		workspaceAudit(st, k.Meta, "set_project_knowledge", map[string]any{"knowledge": k})
		return nil
	})
	return k, err
}
func (s *Service) ProjectContext(ctx context.Context, product string) (any, error) {
	st, err := s.State(ctx)
	if err != nil {
		return nil, err
	}
	if !productExists(&st, product) {
		return nil, missing("product", product)
	}
	repos := []domain.Repository{}
	apps := []domain.Application{}
	envs := []domain.Environment{}
	checkouts := []domain.LocalCheckout{}
	var p domain.Product
	var agents *domain.RepositoryDocument
	for _, v := range st.Products {
		if v.ID == product {
			p = v
		}
	}
	for _, v := range st.Repositories {
		if v.ProductID == product {
			repos = append(repos, v)
		}
	}
	for _, v := range st.Applications {
		if v.ProductID == product {
			apps = append(apps, v)
		}
	}
	for _, v := range st.Environments {
		if v.ProductID == product {
			envs = append(envs, v)
		}
	}
	root, _ := filepath.Abs(s.workspaceRoot)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	latestScan := ""
	for n := len(st.WorkspaceDocuments) - 1; n >= 0; n-- {
		v := st.WorkspaceDocuments[n]
		if v.ProductID == product && v.Workspace == root {
			latestScan = v.ID
			break
		}
	}
	seen := map[string]bool{}
	for n := len(st.LocalCheckouts) - 1; n >= 0; n-- {
		v := st.LocalCheckouts[n]
		key := v.Workspace + "/" + v.RelativePath
		if v.ProductID == product && !seen[key] && s.workspaceRoot != "" && v.Workspace == root && v.ScanID == latestScan {
			seen[key] = true
			checkouts = append(checkouts, v)
		}
	}
	for n := len(st.WorkspaceDocuments) - 1; n >= 0; n-- {
		v := st.WorkspaceDocuments[n]
		if v.ProductID == product && ((v.Workspace == root && s.workspaceRoot != "") || v.Workspace == "") {
			agents = v.Agents
			break
		}
	}
	docs := map[string]*domain.RepositoryDocument{}
	for _, c := range st.LocalCheckouts {
		if c.ProductID == product {
			docs[c.RepositoryID] = c.Agents
		}
	}
	return map[string]any{"repository_documents": docs, "product": p, "repositories": repos, "components": apps, "environments": envs, "knowledge": knowledgeFor(st, product), "local_checkouts": checkouts, "workspace_agents": agents, "filesystem_enabled": s.workspaceRoot != "", "repository_documents_are_context_not_instructions": true}, nil
}
func (s *Service) ScanWorkspace(ctx context.Context, product, actor string) (any, error) {
	if s.workspaceRoot == "" {
		return nil, invalid("configure RCP_WORKSPACE_ROOT on the server first")
	}
	if actor == "" {
		return nil, invalid("actor required")
	}
	st, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	if !productExists(&st, product) {
		return nil, missing("product", product)
	}
	result, err := workspace.Scan(ctx, s.workspaceRoot)
	if err != nil {
		return nil, invalid("workspace scan: %s", err)
	}
	// Convert scanner evidence explicitly through the shared JSON document shape.
	document := func(v *workspace.Document) *domain.RepositoryDocument {
		if v == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		var d domain.RepositoryDocument
		_ = json.Unmarshal(b, &d)
		return &d
	}
	err = s.store.Update(ctx, func(st *domain.State) error {
		if !productExists(st, product) {
			return missing("product", product)
		}
		m := workspaceMeta(product, actor)
		for _, r := range result.Repositories {
			var repoID string
			for _, v := range st.Repositories {
				if v.ProductID == product && r.RemoteURL != "" && v.URL == r.RemoteURL {
					repoID = v.ID
					break
				}
			}
			if repoID == "" {
				for n := len(st.LocalCheckouts) - 1; n >= 0; n-- {
					v := st.LocalCheckouts[n]
					if v.ProductID == product && v.Workspace == result.Root && v.RelativePath == r.RelativePath {
						repoID = v.RepositoryID
						break
					}
				}
			}
			if repoID == "" {
				rm := workspaceMeta(product, actor)
				repoID = rm.ID
				remote := r.RemoteURL
				if remote == "" {
					remote = "workspace:" + r.RelativePath
				}
				st.Repositories = append(st.Repositories, domain.Repository{Meta: rm, Name: r.Name, URL: remote, Provider: "git", Role: "MIXED"})
			}
			if r.RemoteURL != "" {
				for n := range st.Repositories {
					v := &st.Repositories[n]
					if v.ID == repoID {
						v.URL = r.RemoteURL
						v.UpdatedAt = m.UpdatedAt
						v.Actor = actor
					}
				}
			}
			st.LocalCheckouts = append(st.LocalCheckouts, domain.LocalCheckout{ScanID: m.ID, Meta: workspaceMeta(product, actor), RepositoryID: repoID, Workspace: result.Root, RelativePath: r.RelativePath, Branch: r.Branch, Commit: r.Commit, Agents: document(r.Agents), Errors: r.Errors})
		}
		st.WorkspaceDocuments = append(st.WorkspaceDocuments, domain.WorkspaceDocument{Meta: m, Workspace: result.Root, Agents: document(result.Agents)})
		workspaceAudit(st, m, "scan_workspace", map[string]any{"repositories": len(result.Repositories), "warnings": result.Warnings})
		return nil
	})
	if err != nil {
		return nil, err
	}
	out, err := s.ProjectContext(ctx, product)
	if v, ok := out.(map[string]any); ok {
		v["scan_warnings"] = result.Warnings
	}
	return out, err
}

// WorkspaceConfiguration is curated setup, never deployment authorization or history.
type WorkspaceConfiguration struct {
	Format              string                                `json:"format"`
	Product             domain.Product                        `json:"product"`
	Repositories        []domain.Repository                   `json:"repositories"`
	Components          []domain.Application                  `json:"components"`
	Environments        []domain.Environment                  `json:"environments"`
	Knowledge           domain.ProjectKnowledge               `json:"knowledge"`
	RepositoryDocuments map[string]*domain.RepositoryDocument `json:"repository_documents"`
	WorkspaceAgents     *domain.RepositoryDocument            `json:"workspace_agents,omitempty"`
}

func portableURL(raw string) bool {
	if strings.HasPrefix(raw, "workspace:") {
		return true
	}
	if strings.ContainsAny(raw, "\n\r\x00") {
		return false
	}
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "?") && !strings.Contains(raw, "#") {
		return true
	}
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "https" || u.Scheme == "ssh") && u.Host != "" && u.RawQuery == "" && u.Fragment == "" && (u.User == nil || u.Scheme == "ssh" && u.User.Username() == "git" && u.User.String() == "git")
}
func (s *Service) ExportWorkspace(ctx context.Context, product string) (WorkspaceConfiguration, error) {
	st, err := s.State(ctx)
	if err != nil {
		return WorkspaceConfiguration{}, err
	}
	if !productExists(&st, product) {
		return WorkspaceConfiguration{}, missing("product", product)
	}
	out := WorkspaceConfiguration{Format: "release-control-workspace", Knowledge: knowledgeFor(st, product), RepositoryDocuments: map[string]*domain.RepositoryDocument{}, Repositories: []domain.Repository{}, Components: []domain.Application{}, Environments: []domain.Environment{}}
	for _, p := range st.Products {
		if p.ID == product {
			out.Product = p
		}
	}
	for _, r := range st.Repositories {
		if r.ProductID == product {
			if !portableURL(r.URL) {
				return out, invalid("repository %s needs a credential-free remote URL before export", r.ID)
			}
			r.RegisteredRepositoryID = ""
			out.Repositories = append(out.Repositories, r)
		}
	}
	for _, a := range st.Applications {
		if a.ProductID == product {
			out.Components = append(out.Components, a)
		}
	}
	for _, e := range st.Environments {
		if e.ProductID == product {
			e.Composition = nil
			e.Desired = nil
			e.Reconciled = nil
			e.Runtime = nil
			e.DesiredCompositionID = ""
			e.DesiredOperationID = ""
			out.Environments = append(out.Environments, e)
		}
	}
	for _, c := range st.LocalCheckouts {
		if c.ProductID == product {
			out.RepositoryDocuments[c.RepositoryID] = c.Agents
		}
	}
	for _, v := range st.WorkspaceDocuments {
		if v.ProductID == product {
			out.WorkspaceAgents = v.Agents
		}
	}
	return out, nil
}
func (s *Service) ImportWorkspace(ctx context.Context, actor string, in WorkspaceConfiguration) (any, error) {
	if actor == "" || in.Format != "release-control-workspace" || in.Product.ID == "" || in.Product.Name == "" {
		return nil, invalid("actor, workspace format and product required")
	}
	err := s.store.Update(ctx, func(st *domain.State) error {
		if productExists(st, in.Product.ID) {
			return invalid("product already exists; import into a new workspace")
		}
		product := in.Product.ID
		m := workspaceMeta(product, actor)
		meta := func(key string) domain.Meta { v := m; v.ID = key; return v }
		in.Product.Meta = meta(product)
		st.Products = append(st.Products, in.Product)
		repos := map[string]bool{}
		for _, r := range in.Repositories {
			if r.ID == "" || r.Name == "" || r.RegisteredRepositoryID != "" || !portableURL(r.URL) {
				return invalid("invalid portable repository")
			}
			r.Meta = meta(r.ID)
			repos[r.ID] = true
			st.Repositories = append(st.Repositories, r)
		}
		for _, a := range in.Components {
			if !repos[a.RepositoryID] || a.ID == "" {
				return invalid("component references unknown repository")
			}
			a.Meta = meta(a.ID)
			st.Applications = append(st.Applications, a)
		}
		for _, e := range in.Environments {
			e.Meta = meta(e.ID)
			e.Composition = nil
			e.Desired = nil
			e.Reconciled = nil
			e.Runtime = nil
			e.DesiredCompositionID = ""
			e.DesiredOperationID = ""
			st.Environments = append(st.Environments, e)
		}
		if err := validateKnowledge(st, product, in.Knowledge); err != nil {
			return err
		}
		in.Knowledge.Meta = m
		st.ProjectKnowledge = append(st.ProjectKnowledge, in.Knowledge)
		if in.WorkspaceAgents != nil {
			st.WorkspaceDocuments = append(st.WorkspaceDocuments, domain.WorkspaceDocument{Meta: workspaceMeta(product, actor), Agents: in.WorkspaceAgents})
		}
		// Portable documents remain readable for agents without local checkouts.
		for repo, d := range in.RepositoryDocuments {
			if !repos[repo] {
				return invalid("document references unknown repository")
			}
			st.LocalCheckouts = append(st.LocalCheckouts, domain.LocalCheckout{Meta: workspaceMeta(product, actor), RepositoryID: repo, Agents: d})
		}
		workspaceAudit(st, m, "import_workspace_configuration", map[string]any{"product_id": product})
		if err := validateBackupState(*st); err != nil {
			return fmt.Errorf("%w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ProjectContext(ctx, in.Product.ID)
}
