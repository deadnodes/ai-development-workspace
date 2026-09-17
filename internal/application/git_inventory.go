package application

import (
	"encoding/json"
	"releasecontrol/internal/domain"
	"sort"
	"strings"
	"time"
)

// unlinkedGitCommits derives an agent-facing queue from persisted repository
// observations. It never guesses a Feature: a commit is linked only when an
// Integration, revision, linked branch, or recorded PR provenance names it.
func unlinkedGitCommits(st domain.State, productID string) []map[string]any {
	linked := map[string]bool{}
	mark := func(repositoryID, sha string) {
		if repositoryID != "" && sha != "" {
			linked[repositoryID+"/"+sha] = true
		}
	}
	for _, in := range st.Integrations {
		if in.ProductID != productID {
			continue
		}
		for _, commit := range in.Commits {
			mark(commit.RepositoryID, commit.SHA)
		}
	}
	for _, revision := range st.IntegrationRevisions {
		if revision.ProductID != productID {
			continue
		}
		mark(revision.RepositoryID, revision.HeadCommit)
		for _, sha := range revision.Commits {
			mark(revision.RepositoryID, sha)
		}
	}
	for _, observation := range st.GitObservations {
		if observation.ProductID != productID || observation.IntegrationID == "" {
			continue
		}
		for _, commit := range observation.Commits {
			mark(observation.RepositoryID, commit.SHA)
		}
	}
	// Provider PR observations are stored in structured progress memories so
	// source/target/merge provenance survives branch deletion.
	for _, memory := range st.Memories {
		if memory.ProductID != productID || memory.IntegrationID == "" {
			continue
		}
		var records []struct {
			RepositoryID string `json:"repository_id"`
			HeadSHA      string `json:"head_sha"`
			MergeSHA     string `json:"merge_sha"`
		}
		if json.Unmarshal([]byte(memory.Body), &records) == nil {
			for _, record := range records {
				mark(record.RepositoryID, record.HeadSHA)
				mark(record.RepositoryID, record.MergeSHA)
			}
		}
	}

	items := map[string]map[string]any{}
	// Walk the full observation history. Compare results normally contain all
	// commits between base and branch head, but retaining older observations
	// also keeps commits visible after a branch is force-updated or deleted.
	for _, observation := range st.GitObservations {
		if observation.ProductID != productID || observation.IntegrationID != "" {
			continue
		}
		for _, commit := range observation.Commits {
			if commit.SHA == "" || linked[observation.RepositoryID+"/"+commit.SHA] {
				continue
			}
			key := observation.RepositoryID + "/" + commit.SHA
			item := items[key]
			if item == nil {
				item = map[string]any{
					"id":               "unlinked:" + key,
					"reason":           "UNLINKED_GIT_COMMIT",
					"product_id":       productID,
					"repository_id":    observation.RepositoryID,
					"repository":       inventoryRepositoryName(st, observation.RepositoryID),
					"commit":           commit.SHA,
					"message":          commit.Message,
					"author":           commit.Author,
					"url":              commit.URL,
					"observed_at":      observation.ObservedAt,
					"branches":         []string{},
					"suggested_action": "Ask the agent to attach this commit to a Feature/Integration.",
				}
				items[key] = item
			} else if observedAt, ok := item["observed_at"].(time.Time); ok && observation.ObservedAt.After(observedAt) {
				item["message"] = commit.Message
				item["author"] = commit.Author
				item["url"] = commit.URL
				item["observed_at"] = observation.ObservedAt
			}
			branches := item["branches"].([]string)
			if !containsString(branches, observation.Branch) {
				item["branches"] = append(branches, observation.Branch)
			}
		}
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		branches := item["branches"].([]string)
		sort.Strings(branches)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i]["id"].(string) < result[j]["id"].(string)
	})
	return result
}

func inventoryRepositoryName(st domain.State, repositoryID string) string {
	for _, repository := range st.Repositories {
		if repository.ID == repositoryID {
			return repository.Name
		}
	}
	for _, binding := range st.RepositoryBindings {
		if binding.RepositoryID == repositoryID {
			return binding.FullName
		}
	}
	return repositoryID
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}
