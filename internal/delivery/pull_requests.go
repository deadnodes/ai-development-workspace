package delivery

import (
	"context"
	"time"
)

// PullRequestObserver reads PR identity even when its source branch was deleted.
// It does not bind that historical source to a deployable branch.
type PullRequestObserver interface {
	ObservePullRequest(context.Context, Connection, string, int) (PullRequestObservation, error)
}
type PullRequestObservation struct {
	Number                                           int
	URL, Title, State, Head, Base, HeadSHA, MergeSHA string
	CreatedAt, MergedAt                              time.Time
}
