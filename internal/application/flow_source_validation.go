package application

import (
	"context"
	"strings"

	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
)

// validateFlowSource checks source ownership against immutable recorded revisions.
// The complete Git commit difference must be declared; ancestry alone would allow
// an integration branch contaminated by unrelated DEV merges to enter a release.
func validateFlowSource(ctx context.Context, provider delivery.Provider, op domain.ExternalOperation, source domain.FlowSource) error {
	if op.CompositionSnapshot == nil || !fullSHA.MatchString(source.Plan.BaseSHA) {
		return invalid("missing pinned composition source")
	}
	if len(source.Plan.HeadSHAs) > 20 {
		return invalid("composition exceeds bounded source selection")
	}
	branches, err := provider.Branches(ctx, source.Connection, source.Plan.Repository)
	if err != nil {
		return err
	}
	mainSHA := ""
	for _, branch := range branches {
		if branch.Name == source.Plan.BaseRef {
			mainSHA = branch.SHA
		}
	}
	if !fullSHA.MatchString(mainSHA) {
		return invalid("configured main branch not observed")
	}
	if source.Plan.BaseSHA != mainSHA {
		ancestry, err := provider.Compare(ctx, source.Connection, source.Plan.Repository, source.Plan.BaseSHA, mainSHA)
		if err != nil {
			return err
		}
		if ancestry.BaseSHA != source.Plan.BaseSHA || ancestry.HeadSHA != mainSHA || ancestry.Behind != 0 {
			return invalid("composition base is not recorded main history")
		}
	}
	expected := map[string]domain.IntegrationRevision{}
	for _, r := range op.CompositionSnapshot.RevisionSnapshots {
		if r.RepositoryID != source.RepositoryID {
			continue
		}
		if r.ProductID != op.ProductID || !fullSHA.MatchString(r.BaseCommit) || !fullSHA.MatchString(r.HeadCommit) || r.Branch == source.Plan.BaseRef || strings.HasPrefix(r.Branch, "generated/") {
			return invalid("source revision must be isolated feature history, not main or generated composition")
		}
		if _, exists := expected[r.HeadCommit]; exists {
			return invalid("multiple integrations claim same source head")
		}
		expected[r.HeadCommit] = r
	}
	seen := map[string]bool{}
	for _, head := range source.Plan.HeadSHAs {
		r, ok := expected[head]
		if !ok || seen[head] {
			return invalid("source selection does not match recorded integration revision")
		}
		seen[head] = true
		if r.BaseCommit != source.Plan.BaseSHA {
			ancestry, err := provider.Compare(ctx, source.Connection, source.Plan.Repository, r.BaseCommit, source.Plan.BaseSHA)
			if err != nil {
				return err
			}
			if ancestry.Behind != 0 || ancestry.BaseSHA != r.BaseCommit || ancestry.HeadSHA != source.Plan.BaseSHA {
				return invalid("integration origin is not an ancestor of selected main")
			}
		}
		difference, err := provider.Compare(ctx, source.Connection, source.Plan.Repository, r.BaseCommit, r.HeadCommit)
		if err != nil {
			return err
		}
		if difference.BaseSHA != r.BaseCommit || difference.HeadSHA != r.HeadCommit || difference.Behind != 0 || difference.Ahead != len(difference.Commits) {
			return invalid("source history is divergent, truncated or lacks exact comparison evidence")
		}
		declared := map[string]bool{}
		for _, sha := range r.Commits {
			if !fullSHA.MatchString(sha) || declared[sha] {
				return invalid("invalid declared source commit ownership")
			}
			declared[sha] = true
		}
		if len(declared) != len(difference.Commits) {
			return invalid("integration commit list must include complete branch history since recorded origin")
		}
		observed := map[string]bool{}
		for _, commit := range difference.Commits {
			if !declared[commit.SHA] || observed[commit.SHA] {
				return invalid("source contains undeclared or duplicate integration commits")
			}
			observed[commit.SHA] = true
		}
	}
	if len(seen) != len(expected) {
		return invalid("source plan omitted recorded integration revisions")
	}
	return nil
}
