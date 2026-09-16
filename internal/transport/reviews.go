package transport

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"time"
)

type reviewService interface {
	SyncReview(context.Context, string, string, string, int, []string, bool) (any, error)
}
type reviewInput struct {
	CommentKeys   []string `json:"comment_keys,omitempty"`
	Preview       bool     `json:"preview,omitempty"`
	Actor         string   `json:"actor"`
	IntegrationID string   `json:"integration_id"`
	RepositoryID  string   `json:"repository_id"`
	PullRequest   int      `json:"pull_request"`
}

func registerReviewRoutes(mux *http.ServeMux, service Service) {
	s, ok := service.(reviewService)
	if !ok {
		return
	}
	mux.HandleFunc("POST /api/reviews/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			write(w, 415, map[string]string{"error": "Use application/json"})
			return
		}
		var in reviewInput
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "Invalid review request"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		out, err := s.SyncReview(ctx, in.Actor, in.IntegrationID, in.RepositoryID, in.PullRequest, in.CommentKeys, in.Preview)
		respond(w, out, err)
	})
}
func registerReviewTools(server *mcp.Server, service Service) {
	s, ok := service.(reviewService)
	if !ok {
		return
	}
	mcp.AddTool(server, &mcp.Tool{Name: "sync_pull_request_review", Description: "Read current Codex P1/P2 review comments from an explicit GitHub PR into persistent findings for an integration. Read-only GitHub access; never requests a review or edits code/comments. Repeat after review latency; no findings is not approval. PR may target dev or main; attribution is explicit. Requires GitHub App Pull requests read and Issues read permissions."}, func(ctx context.Context, _ *mcp.CallToolRequest, in reviewInput) (*mcp.CallToolResult, any, error) {
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		out, err := s.SyncReview(ctx, in.Actor, in.IntegrationID, in.RepositoryID, in.PullRequest, in.CommentKeys, in.Preview)
		return nil, out, err
	})
}
