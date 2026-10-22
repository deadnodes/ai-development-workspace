package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
	"strings"
	"testing"
	"time"
)

type registryBudgetService struct {
	transport.Service
	reports int
}

func (s *registryBudgetService) ClaimWorker(context.Context, workerconfig.Principal, domain.WorkerClaim) (*domain.WorkerLease, error) {
	return nil, nil
}
func (s *registryBudgetService) StartWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	return domain.WorkerLease{}, nil
}
func (s *registryBudgetService) HeartbeatWorker(context.Context, workerconfig.Principal, domain.WorkerAttemptRequest) (domain.WorkerLease, error) {
	return domain.WorkerLease{}, nil
}
func (s *registryBudgetService) ReportWorker(context.Context, workerconfig.Principal, domain.WorkerReport) (domain.WorkerLease, error) {
	s.reports++
	return domain.WorkerLease{}, nil
}

func TestRegistryObservationReportHTTPBudgetAndStrictDecoding(t *testing.T) {
	token := strings.Repeat("o", 40)
	control := strings.Repeat("c", 40)
	recipe := workerconfig.RegistryObserveRecipe{ID: "observe", Revision: "v1", ProductID: "p", RepositoryID: "r", ComponentID: "app", Origin: "https://registry.example", Repository: "team/app", Limits: domain.RegistryObserveLimits{MaxResponseBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxNodes: 64, MaxEdges: 128, MaxDepth: 16, MaxPages: 16, MaxRequests: 512, TimeoutSeconds: 60}}
	recipe.Hash = workerconfig.ObservationHash(recipe)
	authority, e := workerconfig.New(workerconfig.Config{Revision: "cfg", LeaseSeconds: 10, Observations: []workerconfig.RegistryObserveRecipe{recipe}, Credentials: []workerconfig.Credential{{ID: "observer", Principal: "worker/observer", TokenEnv: "OBSERVE_FIXTURE_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"r"}, Capabilities: []string{"registry_observe"}, ExpiresAt: time.Now().Add(time.Hour)}}}, control, func(string) string { return token })
	if e != nil {
		t.Fatal(e)
	}
	service := &registryBudgetService{}
	handler := transport.New(service, transport.Options{Token: control, Workers: authority})
	typed, _ := json.Marshal(domain.WorkerReport{Result: "PASSED", Registry: &domain.WorkerRegistryReport{Scope: domain.RegistryScope{ProductID: "p", RepositoryID: "r"}}})
	plain, _ := json.Marshal(domain.WorkerReport{Result: "PASSED"})
	for _, c := range []struct {
		name, path string
		body       []byte
		accepted   bool
	}{
		{"observer_over16KiB", "report", append(bytes.Repeat([]byte(" "), 20<<10), typed...), true},
		{"source_pub_still16KiB", "report", append(bytes.Repeat([]byte(" "), 20<<10), plain...), false},
		{"registry_over256KiB", "report", append(bytes.Repeat([]byte(" "), domain.RegistryReportMaxBytes), typed...), false},
		{"trailing_object", "report", append(typed, []byte(" {}")...), false},
		{"unknown_field", "report", []byte(`{"registry":{},"provider_payload":"untrusted"}`), false},
		{"claim_still16KiB", "claim", append(bytes.Repeat([]byte(" "), 20<<10), []byte(`{}`)...), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := service.reports
			r := httptest.NewRequest(http.MethodPost, "/api/worker/"+c.path, bytes.NewReader(c.body))
			r.Host = "127.0.0.1"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if c.accepted {
				if w.Code != 200 || service.reports != before+1 {
					t.Fatalf("typed observer bound rejected %d %s", w.Code, w.Body.String())
				}
			} else if w.Code == 200 || service.reports != before {
				t.Fatal("invalid/oversized request reached service", w.Code)
			}
		})
	}
}
