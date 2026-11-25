package transport

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
)

func TestExecutionGroupCreateThroughHTTPWithLocalStore(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := httptest.NewServer(New(application.New(store), Options{}))
	defer server.Close()
	client := server.Client()
	command := func(body string) {
		t.Helper()
		resp, err := client.Post(server.URL+"/api/commands", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("command returned HTTP %d: %s", resp.StatusCode, body)
		}
	}
	command(`{"action":"create_product","actor":"agent","id":"p","data":{"name":"P"}}`)
	command(`{"action":"create_feature","actor":"agent","id":"f","product_id":"p","data":{"title":"F","goal":"G"}}`)
	for i := 1; i <= 4; i++ {
		command(fmt.Sprintf(`{"action":"create_integration","actor":"agent","id":"i%d","feature_id":"f","data":{"title":"I%d","objective":"O"}}`, i, i))
	}
	command(`{"action":"transition_integration","actor":"agent","integration_id":"i1","data":{"status":"working"}}`)
	command(`{"action":"transition_integration","actor":"agent","integration_id":"i1","data":{"status":"ready"}}`)
	command(`{"action":"create_execution_group","actor":"agent","product_id":"p","data":{"name":"G","orchestrator_id":"orch","integration_ids":["i1","i2","i3","i4"]}}`)
}
