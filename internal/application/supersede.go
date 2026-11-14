package application

import (
	"context"
	"time"

	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
)

type ancestrySource interface {
	IsAncestor(context.Context, buildconfig.Connection, string, string, string) (bool, error)
}

// Results bind an ID to the observed SHA. Callers must revalidate scope and
// eligible status in Update; errors never authorize superseding a handle.
func checkSuperseded(ctx context.Context, source ancestrySource, connection buildconfig.Connection, repository, head string, candidates map[string]string) map[string]string {
	checked := map[string]string{}
	for id, sha := range candidates {
		if sha == head {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		ancestor, err := source.IsAncestor(checkCtx, connection, repository, sha, head)
		cancel()
		if err == nil && ancestor {
			checked[id] = sha
		}
	}
	return checked
}

func supersedeEventData(st *domain.State, by, sha, head string) {
	data := st.Events[len(st.Events)-1].Data.Data
	data["superseded_by"], data["source_sha"], data["new_source_sha"] = by, sha, head
}
