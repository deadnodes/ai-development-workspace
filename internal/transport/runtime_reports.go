package transport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"releasecontrol/internal/auth"
	rr "releasecontrol/internal/runtimereport"
	"strconv"
)

type runtimeReportService interface {
	ReportRuntime(context.Context, rr.Report) (any, error)
	GetRuntimeReport(context.Context, string, string, string, ...rr.PageRequest) (any, error)
}

func runtimeReportPost(service Service, options Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(runtimeReportService)
		if !ok {
			write(w, 503, map[string]string{"error": "runtime reports unavailable"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var report rr.Report
		if err := dec.Decode(&report); err != nil {
			write(w, 400, map[string]string{"error": "invalid report JSON"})
			return
		}
		if dec.Decode(new(any)) != io.EOF {
			write(w, 400, map[string]string{"error": "expected one JSON object"})
			return
		}
		if p, ok := auth.FromContext(r.Context()); ok && p.ID == "service/prod-runtime-reporter" {
			scopes, err := rr.ParseScopes(options.RuntimeReporterScope)
			allowed := err == nil && scopes[report.ProductID+":"+report.EnvironmentID+":"+report.Cluster]
			if !allowed {
				write(w, 403, map[string]string{"error": "reporter scope forbidden"})
				return
			}
		}
		v, err := s.ReportRuntime(r.Context(), report)
		if errors.Is(err, rr.ErrRateLimit) {
			w.Header().Set("Retry-After", "20")
			write(w, 429, map[string]string{"error": "report rate limit"})
			return
		}
		if errors.Is(err, rr.ErrTooLarge) {
			write(w, 413, map[string]string{"error": err.Error()})
			return
		}
		respond(w, v, err)
	}
}
func registerRuntimeReports(mux *http.ServeMux, server *mcp.Server, service Service, options Options) {
	mux.HandleFunc("POST /api/runtime-reports", runtimeReportPost(service, options))
	mux.HandleFunc("GET /api/runtime-reports", func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(runtimeReportService)
		if !ok {
			write(w, 503, nil)
			return
		}
		q := r.URL.Query()
		limit := 0
		if q.Has("limit") {
			var err error
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil || limit < 1 || limit > 1000 {
				write(w, 422, map[string]string{"error": "limit must be 1..1000"})
				return
			}
		}
		v, e := s.GetRuntimeReport(r.Context(), q.Get("product_id"), q.Get("environment_id"), q.Get("cluster"), rr.PageRequest{Limit: limit, Cursor: q.Get("cursor")})
		respond(w, v, e)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_runtime_report", Description: "Read pushed runtime snapshots and cluster freshness; stale after five minutes."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		ProductID     string `json:"product_id"`
		EnvironmentID string `json:"environment_id"`
		Cluster       string `json:"cluster,omitempty"`
		Limit         int    `json:"limit,omitempty"`
		Cursor        string `json:"cursor,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		s, ok := service.(runtimeReportService)
		if !ok {
			return nil, nil, errors.New("runtime reports unavailable")
		}
		v, e := s.GetRuntimeReport(ctx, in.ProductID, in.EnvironmentID, in.Cluster, rr.PageRequest{Limit: in.Limit, Cursor: in.Cursor})
		return nil, v, e
	})
}
