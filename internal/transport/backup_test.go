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
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestPostgresBackupMigrationHTTPMCPAndOrdering(t *testing.T) {
	ctx := context.Background()
	sourceDB := isolatedDatabase(t)
	destDB := isolatedDatabase(t)
	source, err := persistence.Open(ctx, sourceDB)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dest, err := persistence.Open(ctx, destDB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { dest.Close() }()
	svc := application.New(source)
	for _, command := range []domain.Command{
		{Action: "create_product", ID: "p", Data: map[string]any{"name": "Migrating product"}},
		{Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "Intent", "goal": "Preserve"}},
		{Action: "record_decision", ID: "z-first", FeatureID: "f", Data: map[string]any{"title": "First", "reason": "Order"}},
		{Action: "record_decision", ID: "a-last", FeatureID: "f", Data: map[string]any{"title": "Second", "reason": "Append"}},
	} {
		command.Actor = "agent"
		if _, err = svc.Execute(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	before, err := source.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srcServer := httptest.NewServer(transport.New(svc, transport.Options{Token: "source"}))
	defer srcServer.Close()
	req, _ := http.NewRequest("POST", srcServer.URL+"/api/backups/export", nil)
	req.Header.Set("X-RCP-Actor", "agent")
	denied, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != 401 {
		t.Fatal("backup not authenticated")
	}
	req.Header.Set("Authorization", "Bearer source")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("export: %s", archive)
	}
	destSvc := application.New(dest)
	server := httptest.NewServer(transport.New(destSvc, transport.Options{}))
	defer server.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "migration", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "restore_backup", Arguments: map[string]any{"actor": "agent/migrate", "archive_base64": base64.StdEncoding.EncodeToString(archive), "sha256": response.Header.Get("X-Backup-SHA256")}})
	if err != nil || result.IsError {
		t.Fatalf("MCP restore: %v %+v", err, result)
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
		t.Fatal("restore audit missing")
	}
	after.Events = after.Events[:len(before.Events)]
	if !reflect.DeepEqual(before, after) {
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(after)
		t.Fatalf("migration changed state/order\n%s\n%s", a, b)
	}
	// A second import is rejected without replacing any existing history.
	rejectReq, _ := http.NewRequest("POST", server.URL+"/api/backups/restore", bytes.NewReader(archive))
	rejectReq.Header.Set("Content-Type", "application/gzip")
	rejectReq.Header.Set("X-RCP-Actor", "agent")
	rejectReq.Header.Set("X-Backup-SHA256", response.Header.Get("X-Backup-SHA256"))
	// The original server's pool was closed for the reopen check: use the reopened service.
	restoredServer := httptest.NewServer(transport.New(application.New(dest), transport.Options{}))
	defer restoredServer.Close()
	rejectReq.URL.Host = restoredServer.Listener.Addr().String()
	rejected, err := http.DefaultClient.Do(rejectReq)
	if err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if rejected.StatusCode != 422 {
		t.Fatalf("nonempty import: %d", rejected.StatusCode)
	}
}
func TestPostgresConcurrentRestoresOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	src, err := persistence.Open(ctx, isolatedDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	source := application.New(src)
	_, err = source.Execute(ctx, domain.Command{Action: "create_product", Actor: "agent", Data: map[string]any{"name": "Source"}})
	if err != nil {
		t.Fatal(err)
	}
	backup, err := source.CreateBackup(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := persistence.Open(ctx, isolatedDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := application.New(dst).RestoreBackup(ctx, "agent", backup.ArchiveBase64, backup.SHA256)
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d imports succeeded", successes)
	}
}
