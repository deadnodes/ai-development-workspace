package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
)

type workerService interface {
	ClaimWorker(context.Context, workerconfig.Principal, domain.WorkerClaim) (*domain.WorkerLease, error)
	StartWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error)
	HeartbeatWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error)
	ReportWorker(context.Context, workerconfig.Principal, domain.WorkerReport) (domain.WorkerLease, error)
}

// Only the fixed machine protocol bypasses control-plane OIDC. Each endpoint
// still authenticates its configured worker credential and capability scope.
func workerEndpoint(path string) bool {
	switch path {
	case workerClaimPath, workerStartPath, workerHeartbeatPath, workerReportPath:
		return true
	}
	return false
}

const (
	workerClaimPath     = "/api/worker/claim"
	workerStartPath     = "/api/worker/start"
	workerHeartbeatPath = "/api/worker/heartbeat"
	workerReportPath    = "/api/worker/report"
)

func registerWorkerRoutes(mux routeRegistrar, service Service, o Options) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		s, ok := service.(workerService)
		if !ok || o.Workers == nil || o.Token == "" {
			write(w, 503, map[string]string{"error": "external workers disabled"})
			return
		}
		p, err := o.Workers.Authenticate(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), time.Now().UTC())
		if err != nil || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			write(w, 401, map[string]string{"error": "worker authentication required"})
			return
		}
		if r.Method != "POST" {
			write(w, 405, map[string]string{"error": "POST required"})
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			write(w, 415, map[string]string{"error": "application/json required"})
			return
		}
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{ID: p.ID(), Kind: "worker"})
		if r.URL.Path == workerReportPath {
			raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, domain.RegistryReportMaxBytes))
			if e != nil {
				write(w, 400, map[string]string{"error": "invalid worker report bounds"})
				return
			}
			var in domain.WorkerReport
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF {
				write(w, 400, map[string]string{"error": "invalid worker report"})
				return
			}
			if len(raw) > 16<<10 && (in.Registry == nil || !p.Allows(in.Registry.Scope.ProductID, in.Registry.Scope.RepositoryID, "registry_observe", time.Now().UTC())) {
				write(w, 400, map[string]string{"error": "worker report exceeds capability bound"})
				return
			}
			v, e := s.ReportWorker(ctx, p, in)
			respond(w, v, e)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		decode := func(v any) bool {
			if dec.Decode(v) != nil || dec.Decode(new(any)) != io.EOF {
				write(w, 400, map[string]string{"error": "invalid worker request"})
				return false
			}
			return true
		}
		switch r.URL.Path {
		case workerClaimPath:
			var in domain.WorkerClaim
			if !decode(&in) {
				return
			}
			v, e := s.ClaimWorker(ctx, p, in)
			respond(w, v, e)
		case workerStartPath, workerHeartbeatPath:
			var in domain.WorkerAttemptRequest
			if !decode(&in) {
				return
			}
			var v domain.WorkerLease
			var e error
			if r.URL.Path == workerStartPath {
				v, e = s.StartWorker(ctx, p, in)
			} else {
				v, e = s.HeartbeatWorker(ctx, p, in)
			}
			respond(w, v, e)
		default:
			write(w, 404, map[string]string{"error": "worker route not found"})
		}
	}
	for _, path := range []string{workerClaimPath, workerStartPath, workerHeartbeatPath, workerReportPath} {
		mux.HandleFunc(http.MethodPost+" "+path, handler)
	}
	mux.HandleFunc("/api/worker/", handler)
}
