package application

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"sync"

	"releasecontrol/internal/domain"
)

type codeMemoryKey struct{ product, repository, branch string }

type codeMemoryRecord struct {
	document []byte
	status   domain.CodeIndexStatus
	actor    string
	history  []domain.CodeIndexStatus
}

type memoryCodeIndexes struct {
	view    func(context.Context, func(*domain.State) error) error
	mutex   sync.RWMutex
	records map[codeMemoryKey]codeMemoryRecord
}

func (m *memoryCodeIndexes) scoped(ctx context.Context, product, repo string, fn func(*domain.State) error) error {
	return m.view(ctx, func(st *domain.State) error {
		p := productByID(*st, product)
		if p == nil || !productVisibleTo(ctx, p) || repo != "" && !attachedCodeRepo(st, product, repo) {
			return domain.ErrReadScopeNotFound
		}
		return fn(st)
	})
}

func attachedCodeRepo(st *domain.State, product, repo string) bool {
	return slices.ContainsFunc(st.Repositories, func(record domain.Repository) bool { return record.ID == repo && record.ProductID == product })
}

func (m *memoryCodeIndexes) PutCodeIndex(ctx context.Context, in domain.CodeIndexWrite, status domain.CodeIndexStatus) (out domain.CodeIndexWriteResult, err error) {
	err = m.scoped(ctx, in.ProductID, in.RepositoryID, func(_ *domain.State) error {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		key := codeMemoryKey{in.ProductID, in.RepositoryID, in.Document.Branch}
		prior := m.records[key]
		if prior.status.Digest == status.Digest {
			out.CodeIndexStatus = prior.status
			out.Truncated = slices.Clone(out.Truncated)
			return nil
		}
		data, err := json.Marshal(in.Document)
		if err != nil {
			return err
		}
		status.Truncated = slices.Clone(status.Truncated)
		history := append([]domain.CodeIndexStatus{status}, prior.history...)
		m.records[key] = codeMemoryRecord{data, status, in.Actor, history[:min(domain.CodeHistorySize, len(history))]}
		out = domain.CodeIndexWriteResult{CodeIndexStatus: status, Changed: true}
		out.Truncated = slices.Clone(status.Truncated)
		return nil
	})
	return out, err
}

func (m *memoryCodeIndexes) CodeStatuses(ctx context.Context, in domain.CodeStatusRequest, limit int) (out []domain.CodeIndexStatus, err error) {
	err = m.scoped(ctx, in.ProductID, in.RepositoryID, func(st *domain.State) error {
		out = m.statuses(st, in, limit)
		return nil
	})
	return out, err
}

func (m *memoryCodeIndexes) statuses(st *domain.State, in domain.CodeStatusRequest, limit int) []domain.CodeIndexStatus {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	var out []domain.CodeIndexStatus
	for key, record := range m.records {
		if key.product == in.ProductID && (in.RepositoryID == "" || in.RepositoryID == key.repository) && (in.Branch == "" || in.Branch == key.branch) && attachedCodeRepo(st, key.product, key.repository) {
			item := record.status
			item.Truncated = slices.Clone(item.Truncated)
			out = append(out, item)
		}
	}
	domain.SortCodeStatuses(out)
	return out[:min(limit, len(out))]
}

func (m *memoryCodeIndexes) deleteProduct(product string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for key := range m.records {
		if key.product == product {
			delete(m.records, key)
		}
	}
}

func codeIndexNewer(candidate, prior domain.CodeIndexStatus) bool {
	return prior.IndexedAt.IsZero() || candidate.IndexedAt.After(prior.IndexedAt) || candidate.IndexedAt.Equal(prior.IndexedAt) && candidate.Branch < prior.Branch
}

func (m *memoryCodeIndexes) ReadCodeIndex(ctx context.Context, product, repo, branch string) (out domain.CodeIndex, err error) {
	err = m.scoped(ctx, product, repo, func(_ *domain.State) error {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		var selected codeMemoryRecord
		for key, record := range m.records {
			if key.product == product && key.repository == repo && (branch == "" || key.branch == branch) && codeIndexNewer(record.status, selected.status) {
				selected = record
			}
		}
		if selected.document == nil {
			return domain.ErrReadScopeNotFound
		}
		return json.Unmarshal(selected.document, &out)
	})
	return out, err
}

func (m *memoryCodeIndexes) FindCode(ctx context.Context, in domain.CodeSearchRequest) ([]domain.CodeCard, error) {
	type scored struct {
		card  domain.CodeCard
		score int
	}
	best := make([]scored, 0, in.Limit+2)
	err := m.scoped(ctx, in.ProductID, in.RepositoryID, func(st *domain.State) error {
		m.mutex.RLock()
		defer m.mutex.RUnlock()
		latest := map[string]codeMemoryRecord{}
		for key, record := range m.records {
			if key.product == in.ProductID && (in.RepositoryID == "" || key.repository == in.RepositoryID) && attachedCodeRepo(st, key.product, key.repository) && codeIndexNewer(record.status, latest[key.repository].status) {
				latest[key.repository] = record
			}
		}
		for _, record := range latest {
			var doc domain.CodeIndex
			if err := json.Unmarshal(record.document, &doc); err != nil {
				return err
			}
			doc.SearchEntries(func(entry domain.CodeSearchEntry) bool {
				if in.Kind != "" && entry.Card.Kind != in.Kind {
					return true
				}
				score := entry.Score(in.Query)
				if score == 0 {
					return true
				}
				position := sort.Search(len(best), func(index int) bool {
					return cmp.Or(cmp.Compare(best[index].score, score), domain.CompareCodeCards(entry.Card, best[index].card)) < 0
				})
				if position <= in.Limit {
					entry.Card.Snippet = domain.CodeSnippet(entry.Snippet, in.Query)
					best = slices.Insert(best, position, scored{entry.Card, score})
					best = best[:min(in.Limit+1, len(best))]
				}
				return true
			})
		}
		return nil
	})
	out := make([]domain.CodeCard, len(best))
	for index, item := range best {
		out[index] = item.card
	}
	return out, err
}
