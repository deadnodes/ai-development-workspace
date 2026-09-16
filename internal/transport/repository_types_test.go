package transport_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestPostgresLibraryMetadataHTTPMCPBackupAndReopen(t *testing.T) {
	ctx := context.Background()
	sourceDB, destDB := isolatedDatabase(t), isolatedDatabase(t)
	source, err := persistence.Open(ctx, sourceDB)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	svc := application.New(source)
	server := httptest.NewServer(transport.New(svc, transport.Options{}))
	defer server.Close()
	post := func(c domain.Command) {
		t.Helper()
		c.Actor = "agent/http"
		body, _ := json.Marshal(c)
		res, err := http.Post(server.URL+"/api/commands", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", c.Action, res.StatusCode, out)
		}
	}
	post(domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "SDK product"}})
	post(domain.Command{Action: "create_repository", ID: "repo", ProductID: "p", Data: map[string]any{"name": "SDK source", "url": "https://example.test/sdk", "role": "LIBRARY"}})
	post(domain.Command{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "SDK compatibility", "goal": "Preserve contracts"}})
	post(domain.Command{Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "Publish SDK", "objective": "Independent package release"}})
	session, err := mcp.NewClient(&mcp.Implementation{Name: "library-agent", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, c := range []domain.Command{
		{Action: "create_application", ID: "library", ProductID: "p", Data: map[string]any{"name": "SDK", "repository_id": "repo", "kind": "LIBRARY"}},
		{Action: "configure_publication", ID: "publication", ProductID: "p", Data: map[string]any{"application_id": "library", "format": "npm", "registry_url": "https://registry.example.test", "package_name": "@team/sdk"}},
		{Action: "record_package_artifact", ID: "package", ProductID: "p", IntegrationID: "i", Data: map[string]any{"application_id": "library", "publication_target_id": "publication", "source_commit": strings.Repeat("a", 40), "version": "1.0.0", "checksum": "sha256:" + strings.Repeat("b", 64), "uri": "https://registry.example.test/sdk-1.0.0.tgz"}},
	} {
		c.Actor = "agent/mcp"
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: c})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %+v", c.Action, err, result)
		}
	}
	before, err := svc.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Environments) != 0 || len(before.ComponentBuilds) != 0 || len(before.PackageArtifacts) != 1 {
		t.Fatal("library created fake deployment state or lost publication")
	}
	contextValue, err := svc.Resume(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	contextJSON, _ := json.Marshal(contextValue)
	if !bytes.Contains(contextJSON, []byte("sdk-1.0.0.tgz")) {
		t.Fatal("agent context lost linked package publication")
	}
	response, err := http.Get(server.URL + "/api/products/p/configuration")
	if err != nil {
		t.Fatal(err)
	}
	var configuration application.ProductConfiguration
	json.NewDecoder(response.Body).Decode(&configuration)
	response.Body.Close()
	if len(configuration.Configuration["publication_targets"]) != 1 || configuration.Configuration["applications"][0]["kind"] != "LIBRARY" {
		t.Fatal("configuration mirror omitted library setup")
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/backups/export", nil)
	req.Header.Set("X-RCP-Actor", "agent/backup")
	archiveResponse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(archiveResponse.Body)
	archiveResponse.Body.Close()
	if archiveResponse.StatusCode != 200 {
		t.Fatalf("backup: %s", archive)
	}
	dest, err := persistence.Open(ctx, destDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { dest.Close() }()
	destServer := httptest.NewServer(transport.New(application.New(dest), transport.Options{}))
	defer destServer.Close()
	destSession, err := mcp.NewClient(&mcp.Implementation{Name: "migration", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: destServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer destSession.Close()
	restored, err := destSession.CallTool(ctx, &mcp.CallToolParams{Name: "restore_backup", Arguments: map[string]any{"actor": "agent/migrate", "archive_base64": base64.StdEncoding.EncodeToString(archive), "sha256": archiveResponse.Header.Get("X-Backup-SHA256")}})
	if err != nil || restored.IsError {
		t.Fatalf("restore: %v %+v", err, restored)
	}
	dest.Close()
	dest, err = persistence.Open(ctx, destDB)
	if err != nil {
		t.Fatal(err)
	}
	after, err := dest.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events)+1 {
		t.Fatal("migration audit missing")
	}
	after.Events = after.Events[:len(before.Events)]
	if !reflect.DeepEqual(before, after) {
		t.Fatal("library configuration or publication history changed across backup/reopen")
	}
}
