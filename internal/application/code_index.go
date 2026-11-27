package application

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"releasecontrol/internal/domain"
)

type codeIndexStore interface {
	PutCodeIndex(context.Context, domain.CodeIndexWrite, domain.CodeIndexStatus) (domain.CodeIndexWriteResult, error)
	CodeStatuses(context.Context, domain.CodeStatusRequest, int) ([]domain.CodeIndexStatus, error)
	ReadCodeIndex(context.Context, string, string, string) (domain.CodeIndex, error)
	FindCode(context.Context, domain.CodeSearchRequest) ([]domain.CodeCard, error)
}

func (s *Service) codeStore() codeIndexStore {
	if store, ok := s.store.(codeIndexStore); ok {
		return store
	}
	return s.codeMemory
}

func (s *Service) codeScope(ctx context.Context, permission, product, repository string) error {
	if !stateID(product) || repository != "" && !stateID(repository) {
		return invalid("exact product and repository IDs required")
	}
	if err := s.requireProductAccess(ctx, product); err != nil {
		return err
	}
	return authorize(ctx, permission, product)
}

func codeScopeError(err error) error {
	if errors.Is(err, domain.ErrReadScopeNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Service) ListCodeRepositories(ctx context.Context, product, cursor string) (domain.CodeRepositoryPage, error) {
	if err := s.codeScope(ctx, "read", product, ""); err != nil {
		return domain.CodeRepositoryPage{}, err
	}
	after := ""
	if cursor != "" {
		if len(cursor) > 1024 {
			return domain.CodeRepositoryPage{}, invalid("invalid repository cursor")
		}
		scope, encoded, found := strings.Cut(cursor, ".")
		last, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
		if !found || scope != encodeCursorID(product) || err != nil || !stateID(string(last)) {
			return domain.CodeRepositoryPage{}, invalid("invalid repository cursor for Product")
		}
		after = string(last)
	}
	const limit = domain.CodeRepositoryPageSize + 1
	out := domain.CodeRepositoryPage{ProductID: product, Items: []domain.CodeRepository{}}
	if reader, ok := s.store.(interface {
		ReadCodeRepositories(context.Context, string, string, int) ([]domain.CodeRepository, error)
	}); ok {
		items, err := reader.ReadCodeRepositories(ctx, product, after, limit)
		if err != nil {
			return domain.CodeRepositoryPage{}, codeScopeError(err)
		}
		out.Items = append(out.Items, items...)
	} else if err := s.view(ctx, func(st *domain.State) error {
		if record := productByID(*st, product); record == nil || !productVisibleTo(ctx, record) {
			return ErrNotFound
		}
		for _, repo := range st.Repositories {
			if repo.ProductID != product || repo.ID <= after {
				continue
			}
			position := sort.Search(len(out.Items), func(index int) bool { return out.Items[index].ID >= repo.ID })
			if position < limit {
				out.Items = slices.Insert(out.Items, position, domain.CodeRepository{ID: repo.ID, URL: repo.URL})
				out.Items = out.Items[:min(limit, len(out.Items))]
			}
		}
		return nil
	}); err != nil {
		return domain.CodeRepositoryPage{}, err
	}
	if len(out.Items) == limit {
		out.Items = out.Items[:domain.CodeRepositoryPageSize]
		out.NextCursor = encodeCursorID(product) + "." + encodeCursorID(out.Items[len(out.Items)-1].ID)
	}
	for _, item := range out.Items {
		if !stateID(item.ID) || len(item.URL) > domain.CodeRepositoryURLBytes {
			return domain.CodeRepositoryPage{}, invalid("invalid or oversized repository identity or URL")
		}
	}
	return out, nil
}

func (s *Service) UpsertCodeIndex(ctx context.Context, in domain.CodeIndexWrite) (domain.CodeIndexWriteResult, error) {
	if err := s.codeScope(ctx, "write", in.ProductID, in.RepositoryID); err != nil {
		return domain.CodeIndexWriteResult{}, err
	}
	if in.RepositoryID == "" {
		return domain.CodeIndexWriteResult{}, invalid("repository_id is required")
	}
	if err := in.Document.Validate(in.RepositoryID); err != nil {
		return domain.CodeIndexWriteResult{}, invalid("%s", err)
	}
	in.Actor = attributedActor(ctx, in.Actor)
	if in.Actor == "" {
		in.Actor = "agent"
	}
	if len(in.Actor) > 512 || !utf8.ValidString(in.Actor) || strings.ContainsFunc(in.Actor, unicode.IsControl) || domain.CodeHasSecret(in.Actor) {
		return domain.CodeIndexWriteResult{}, invalid("invalid or sensitive actor")
	}
	digest, err := in.Document.Digest()
	if err != nil {
		return domain.CodeIndexWriteResult{}, err
	}
	status := domain.CodeIndexStatus{RepositoryID: in.RepositoryID, Branch: in.Document.Branch, Commit: in.Document.Commit, Digest: digest, IndexedAt: time.Now().UTC(), Truncated: in.Document.Truncated}
	result, err := s.codeStore().PutCodeIndex(ctx, in, status)
	result.AgeSeconds = codeAge(result.IndexedAt)
	return result, codeScopeError(err)
}

func codeAge(stamp time.Time) int64 { return max(0, int64(time.Since(stamp)/time.Second)) }

func validCodeBranch(branch string) bool {
	return len(branch) <= 256 && utf8.ValidString(branch) && !strings.ContainsFunc(branch, unicode.IsControl)
}

func (s *Service) GetCodeIndexStatus(ctx context.Context, in domain.CodeStatusRequest) (domain.CodeStatusResult, error) {
	result := domain.CodeStatusResult{Items: []domain.CodeIndexStatus{}}
	if err := s.codeScope(ctx, "read", in.ProductID, in.RepositoryID); err != nil {
		return result, err
	}
	if !validCodeBranch(in.Branch) || in.Branch != "" && in.RepositoryID == "" {
		return result, invalid("branch requires an exact repository_id")
	}
	items, err := s.codeStore().CodeStatuses(ctx, in, 201)
	if err != nil {
		return result, codeScopeError(err)
	}
	return codeStatusResult(items), nil
}

func (s *Service) projectCodeStatus(ctx context.Context, st domain.State, product string) (domain.CodeStatusResult, error) {
	if _, persistent := s.store.(codeIndexStore); persistent {
		return s.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: product})
	}
	return codeStatusResult(s.codeMemory.statuses(&st, domain.CodeStatusRequest{ProductID: product}, 201)), nil
}

func codeStatusResult(items []domain.CodeIndexStatus) domain.CodeStatusResult {
	result := domain.CodeStatusResult{Items: []domain.CodeIndexStatus{}}
	used := 128
	for _, item := range items {
		item.AgeSeconds = codeAge(item.IndexedAt)
		if len(result.Items) == 200 || !appendWithin(&result.Items, item, &used, 64<<10) {
			result.Truncated = true
			break
		}
	}
	return result
}

type codeMapItem struct {
	Path         string                  `json:"path"`
	Directory    bool                    `json:"directory,omitempty"`
	Language     string                  `json:"language,omitempty"`
	Languages    map[string]int          `json:"languages,omitempty"`
	Size         int64                   `json:"size"`
	Lines        int                     `json:"lines"`
	SymbolCount  int                     `json:"symbol_count"`
	Files        int                     `json:"file_count"`
	Dependencies []domain.CodeDependency `json:"dependencies,omitempty"`
	Entrypoints  []domain.CodeEntry      `json:"entry_points,omitempty"`
}

type codeMapResult struct {
	RepositoryID   string        `json:"repository_id"`
	Branch         string        `json:"branch"`
	Commit         string        `json:"commit"`
	IndexTruncated []string      `json:"index_truncated,omitempty"`
	Items          []codeMapItem `json:"items"`
	Manifests      []codeMapItem `json:"manifests"`
	Entrypoints    []codeMapItem `json:"entry_points"`
	Truncated      bool          `json:"truncated"`
}

func appendWithin[Item any](items *[]Item, item Item, used *int, budget int) bool {
	data, err := json.Marshal(item)
	if err != nil || *used+len(data)+1 > budget {
		return false
	}
	*used += len(data) + 1
	*items = append(*items, item)
	return true
}

func (s *Service) GetCodeMap(ctx context.Context, in domain.CodeMapRequest) (any, error) {
	if err := s.codeScope(ctx, "read", in.ProductID, in.RepositoryID); err != nil {
		return nil, err
	}
	if in.Depth == 0 {
		in.Depth = 3
	}
	if in.Limit == 0 {
		in.Limit = 100
	}
	prefix := strings.TrimSuffix(in.PathPrefix, "/")
	if in.RepositoryID == "" || !validCodeBranch(in.Branch) || in.Depth < 1 || in.Depth > 32 || in.Limit < 1 || in.Limit > 500 || in.PathPrefix != "" && !domain.CodePathValid(prefix) {
		return nil, invalid("invalid repository, branch, path_prefix, depth (1..32) or limit (1..500)")
	}
	doc, err := s.codeStore().ReadCodeIndex(ctx, in.ProductID, in.RepositoryID, in.Branch)
	if err != nil {
		return nil, codeScopeError(err)
	}
	out := codeMapResult{RepositoryID: in.RepositoryID, Branch: doc.Branch, Commit: doc.Commit, IndexTruncated: doc.Truncated, Items: []codeMapItem{}, Manifests: []codeMapItem{}, Entrypoints: []codeMapItem{}}
	header, _ := json.Marshal(out)
	used := len(header)
	nodes := map[string]*codeMapItem{}
	order := []string{}
	selected := []domain.CodeFile{}
	for _, file := range doc.Files {
		if prefix != "" && file.Path != prefix && !strings.HasPrefix(file.Path, prefix+"/") {
			continue
		}
		selected = append(selected, file)
		parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(file.Path, prefix), "/"), "/")
		if file.Path == prefix {
			parts = []string{path.Base(prefix)}
		}
		for depth := 1; depth <= min(in.Depth, len(parts)); depth++ {
			name := path.Join(prefix, strings.Join(parts[:depth], "/"))
			if file.Path == prefix {
				name = prefix
			}
			node := nodes[name]
			if node == nil {
				if len(order) == in.Limit {
					out.Truncated = true
					break
				}
				node = &codeMapItem{Path: name, Directory: depth < len(parts)}
				if node.Directory {
					node.Languages = map[string]int{}
				} else {
					node.Language = file.Language
				}
				nodes[name] = node
				order = append(order, name)
			}
			node.Size += int64(file.Size)
			node.Lines += file.Lines
			node.SymbolCount += len(file.Symbols)
			node.Files++
			if node.Directory {
				node.Languages[file.Language]++
			}
		}
		if len(parts) > in.Depth {
			out.Truncated = true
		}
	}
	for _, name := range order {
		if len(out.Items) == in.Limit || !appendWithin(&out.Items, *nodes[name], &used, 48<<10) {
			out.Truncated = true
			break
		}
	}
	for _, file := range selected {
		if len(file.Dependencies) > 0 {
			for _, dep := range file.Dependencies {
				if len(out.Manifests) == in.Limit || !appendWithin(&out.Manifests, codeMapItem{Path: file.Path, Dependencies: []domain.CodeDependency{dep}}, &used, 64<<10) {
					out.Truncated = true
					break
				}
			}
		}
		for _, entry := range file.Entrypoints {
			if len(out.Entrypoints) == in.Limit || !appendWithin(&out.Entrypoints, codeMapItem{Path: file.Path, Entrypoints: []domain.CodeEntry{entry}}, &used, 64<<10) {
				out.Truncated = true
				break
			}
		}
	}
	return out, nil
}

func (s *Service) GetCodeFileOutline(ctx context.Context, in domain.CodeOutlineRequest) (any, error) {
	if err := s.codeScope(ctx, "read", in.ProductID, in.RepositoryID); err != nil {
		return nil, err
	}
	if in.RepositoryID == "" || !domain.CodePathValid(in.Path) {
		return nil, invalid("exact repository_id and relative path required")
	}
	doc, err := s.codeStore().ReadCodeIndex(ctx, in.ProductID, in.RepositoryID, "")
	if err != nil {
		return nil, codeScopeError(err)
	}
	for _, file := range doc.Files {
		if file.Path != in.Path {
			continue
		}
		outline := domain.CodeFile{Path: file.Path, Language: file.Language, Size: file.Size, Lines: file.Lines, Doc: domain.CodePrefix(file.Doc, 4096)}
		truncated := len(outline.Doc) != len(file.Doc)
		used := 32 << 10
		for _, symbol := range file.Symbols {
			if !appendWithin(&outline.Symbols, symbol, &used, 62<<10) {
				truncated = true
				break
			}
		}
		for _, route := range file.Routes {
			if !appendWithin(&outline.Routes, route, &used, 64<<10) {
				truncated = true
				break
			}
		}
		return map[string]any{"repository_id": doc.RepositoryID, "branch": doc.Branch, "commit": doc.Commit, "file": outline, "truncated": truncated, "index_truncated": doc.Truncated}, nil
	}
	return nil, ErrNotFound
}

func (s *Service) SearchCode(ctx context.Context, in domain.CodeSearchRequest) (domain.CodeSearchResult, error) {
	out := domain.CodeSearchResult{Items: []domain.CodeCard{}}
	if err := s.codeScope(ctx, "read", in.ProductID, in.RepositoryID); err != nil {
		return out, err
	}
	in.Query = strings.ToLower(strings.TrimSpace(in.Query))
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Query == "" || !utf8.ValidString(in.Query) || strings.ContainsRune(in.Query, 0) || utf8.RuneCountInString(in.Query) > 256 || in.Limit < 1 || in.Limit > 50 {
		return out, invalid("query requires 1..256 characters and limit must be 1..50")
	}
	switch in.Kind {
	case "", "file", "symbol", "route", "doc", "dependency":
	default:
		return out, invalid("invalid code search kind")
	}
	items, err := s.codeStore().FindCode(ctx, in)
	if err != nil {
		return out, codeScopeError(err)
	}
	used := 128
	for _, item := range items {
		if len(out.Items) == in.Limit || !appendWithin(&out.Items, item, &used, 64<<10) {
			out.Truncated = true
			break
		}
	}
	return out, nil
}
