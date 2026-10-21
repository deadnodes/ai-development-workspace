package transport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/workerconfig"
	"strings"
	"time"
)

type workerService interface {
	ClaimWorker(context.Context, workerconfig.Principal, domain.WorkerClaim) (*domain.WorkerLease, error)
	StartWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error)
	HeartbeatWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error)
	ReportWorker(context.Context, workerconfig.Principal, domain.WorkerReport) (domain.WorkerLease, error)
}

func registerWorkerRoutes(mux *http.ServeMux, service Service, o Options) {
	mux.HandleFunc("/api/worker/", func(w http.ResponseWriter, r *http.Request) {
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
		case "/api/worker/claim":
			var in domain.WorkerClaim
			if !decode(&in) {
				return
			}
			v, e := s.ClaimWorker(r.Context(), p, in)
			respond(w, v, e)
		case "/api/worker/start", "/api/worker/heartbeat":
			var in domain.WorkerAttemptRequest
			if !decode(&in) {
				return
			}
			var v domain.WorkerLease
			var e error
			if r.URL.Path == "/api/worker/start" {
				v, e = s.StartWorker(r.Context(), p, in)
			} else {
				v, e = s.HeartbeatWorker(r.Context(), p, in)
			}
			respond(w, v, e)
		case "/api/worker/report":
			var in domain.WorkerReport
			if !decode(&in) {
				return
			}
			v, e := s.ReportWorker(r.Context(), p, in)
			respond(w, v, e)
		default:
			write(w, 404, map[string]string{"error": "worker route not found"})
		}
	})
}
