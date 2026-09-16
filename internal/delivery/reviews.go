package delivery

import (
	"context"
	"time"
)

type ReviewComment struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
type PullRequestReview struct {
	Number   int             `json:"number"`
	URL      string          `json:"url"`
	Head     string          `json:"head"`
	Base     string          `json:"base"`
	HeadSHA  string          `json:"head_sha"`
	State    string          `json:"state"`
	Comments []ReviewComment `json:"comments"`
}
type ReviewProvider interface {
	PullRequestReview(context.Context, Connection, string, int) (PullRequestReview, error)
}
