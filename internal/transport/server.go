package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/web"
)

type Service interface {
	State(context.Context) (domain.State, error)
	Execute(context.Context, domain.Command) (any, error)
	Resume(context.Context, string) (any, error)
}

type Options struct {
	Token        string
	AllowedHosts []string
}

func New(service Service, options Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { v, e := service.State(r.Context()); respond(w, v, e) })
	mux.HandleFunc("GET /api/schema", func(w http.ResponseWriter, r *http.Request) { write(w, http.StatusOK, CommandSchema()) })
	mux.HandleFunc("GET /api/features/{id}/context", func(w http.ResponseWriter, r *http.Request) {
		v, e := service.Resume(r.Context(), r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/commands", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "Use Content-Type: application/json"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var c domain.Command
		if e := dec.Decode(&c); e != nil {
			write(w, 400, map[string]string{"error": "Invalid command JSON: " + e.Error()})
			return
		}
		if e := dec.Decode(new(any)); e != io.EOF {
			write(w, 400, map[string]string{"error": "Expected one JSON object"})
			return
		}
		v, e := service.Execute(r.Context(), c)
		respond(w, v, e)
	})
	server := mcp.NewServer(&mcp.Implementation{Name: "release-control", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "get_state", Description: "Read products, features, integration/work claims, verification, findings, applications, environments, immutable source revisions, composition plans, releases and audit history."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		v, e := service.State(ctx)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "resume", Description: "Deterministic feature context for a fresh coding agent: intent, ordered plan, progress, decisions, discoveries, blockers, handoffs, verification evidence and other active work."}, func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
		FeatureID string `json:"feature_id" jsonschema:"Feature ID to resume"`
	}) (*mcp.CallToolResult, any, error) {
		v, e := service.Resume(ctx, in.FeatureID)
		return nil, v, e
	})
	mcp.AddTool(server, &mcp.Tool{Name: "execute", Description: "Record one attributed engineering action atomically. Inspect the action-specific input schema. Completion means ready, not released. Check results and handoffs are historical; resolving findings requires a passing rerun for blocking gates. Use create_application, record_integration_revision, plan_composition and select_composition for environment planning. Selecting a composition sets desired intent only; it never merges, builds or deploys. Revision commits are full SHAs supplied by the caller, not yet verified by Git. IDs are optional on creates; reuse returned IDs.", InputSchema: CommandSchema()}, func(ctx context.Context, _ *mcp.CallToolRequest, in domain.Command) (*mcp.CallToolResult, any, error) {
		v, e := service.Execute(ctx, in)
		return nil, v, e
	})
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20}))
	mux.Handle("/", http.FileServer(http.FS(web.FS)))
	return protect(mux, options)
}

func respond(w http.ResponseWriter, v any, err error) {
	if err == nil {
		write(w, 200, v)
		return
	}
	code := http.StatusInternalServerError
	message := "Internal error"
	switch {
	case errors.Is(err, application.ErrNotFound):
		code = 404
		message = err.Error()
	case errors.Is(err, application.ErrValidation):
		code = 422
		message = err.Error()
	default:
		slog.Error("request failed", "error", err)
	}
	write(w, code, map[string]string{"error": message})
}
func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func protect(next http.Handler, o Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		host := r.Host
		if h, _, e := net.SplitHostPort(host); e == nil {
			host = h
		}
		allowed := host == "localhost" || host == "127.0.0.1" || host == "::1"
		for _, h := range o.AllowedHosts {
			if strings.EqualFold(host, h) {
				allowed = true
			}
		}
		if !allowed {
			write(w, 403, map[string]string{"error": "Host not allowed; configure ALLOWED_HOSTS"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				write(w, 403, map[string]string{"error": "Cross-origin requests are not allowed"})
				return
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			write(w, 403, map[string]string{"error": "Cross-site requests are not allowed"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
			w.Header().Set("Cache-Control", "no-store")
			if o.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+o.Token)) != 1 {
				write(w, 401, map[string]string{"error": "Bearer token required"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
