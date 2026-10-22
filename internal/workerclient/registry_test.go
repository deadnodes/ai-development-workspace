package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"releasecontrol/internal/domain"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRegistryObserverResponseBoundIsSeparateFromSourceClient(t *testing.T) {
	nodes := []domain.RegistryNode{}
	for i := 0; i < 800; i++ {
		nodes = append(nodes, domain.RegistryNode{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(fmt.Sprint(i)))), Kind: "BLOB", MediaType: "application/octet-stream", Availability: "PRESENT", Size: 1})
	}
	lease := domain.WorkerLease{Job: domain.WorkerJob{Capability: "registry_observe"}, Attempt: domain.WorkerAttempt{Report: &domain.WorkerReport{Result: "PASSED", Registry: &domain.WorkerRegistryReport{Graph: domain.RegistryGraph{Nodes: nodes}}}}}
	payload, _ := json.Marshal(lease)
	if len(payload) <= 64<<10 || len(payload) >= 256<<10 {
		t.Fatal("fixture bound")
	}
	var mode atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("o", 32) {
			t.Error("wrong workerchannel")
		}
		switch mode.Load() {
		case 1:
			w.Write(payload)
			w.Write([]byte(strings.Repeat(" ", 512<<10)))
		case 2:
			w.Write([]byte(`{"job":{},"attempt":{},"surprise":"untrusted"}`))
		default:
			w.Write(payload)
		}
	}))
	defer s.Close()
	source, e := New(s.URL, strings.Repeat("o", 32))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = source.Claim(context.Background(), domain.WorkerClaim{}); e == nil {
		t.Fatal("source response bound weakened")
	}
	observer, e := NewRegistryObserver(s.URL, strings.Repeat("o", 32))
	if e != nil {
		t.Fatal(e)
	}
	if out, e := observer.Claim(context.Background(), domain.WorkerClaim{Capability: "registry_observe"}); e != nil || len(out.Attempt.Report.Registry.Graph.Nodes) != 800 {
		t.Fatal("typed registry response rejected", e)
	}
	for _, value := range []int64{1, 2} {
		mode.Store(value)
		if _, e = observer.Claim(context.Background(), domain.WorkerClaim{Capability: "registry_observe"}); e == nil {
			t.Fatal("oversized/unknown registry response accepted")
		}
	}
}
