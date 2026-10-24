package transport_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	bolt "go.etcd.io/bbolt"
	"releasecontrol/internal/application"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

// Any provider method promoted from this nil interface panics. Inventory and
// pins must work without touching it, including through HTTP/MCP/attention.
type retentionNoIOProvider struct{ delivery.Provider }
type retentionAuthTransport struct{ token string }

func (t retentionAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

type retentionTestStore interface {
	application.Store
	Close()
}

func TestRetentionInventoryProtectedAPIReopenBackupRestore(t *testing.T) {
	for _, kind := range []string{"local", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "pins.db")
			db := ""
			if kind == "postgres" {
				db = isolatedDatabase(t)
			}
			open := func() retentionTestStore {
				t.Helper()
				if kind == "local" {
					s, e := persistence.OpenLocal(ctx, path)
					if e != nil {
						t.Fatal(e)
					}
					return s
				}
				s, e := persistence.Open(ctx, db)
				if e != nil {
					t.Fatal(e)
				}
				return s
			}
			store := open()
			defer func() { store.Close() }()
			service := application.NewWithProvider(store, &retentionNoIOProvider{})
			token := "retention-fixture-control-token-0000000001"
			server := httptest.NewServer(transport.New(service, transport.Options{Token: token}))
			defer server.Close()
			request := func(endpoint, method, path string, body []byte, auth bool, headers map[string]string) (int, []byte, http.Header) {
				t.Helper()
				r, _ := http.NewRequest(method, endpoint+path, bytes.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				if auth {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				for k, v := range headers {
					r.Header.Set(k, v)
				}
				response, e := http.DefaultClient.Do(r)
				if e != nil {
					t.Fatal(e)
				}
				defer response.Body.Close()
				b, _ := io.ReadAll(response.Body)
				return response.StatusCode, b, response.Header
			}
			post := func(c domain.Command, expected int, out any) {
				t.Helper()
				c.Actor = "fixture/operator"
				b, _ := json.Marshal(c)
				status, body, _ := request(server.URL, "POST", "/api/commands", b, true, nil)
				if status != expected {
					t.Fatalf("%s status%d:%s", c.Action, status, body)
				}
				if out != nil && json.Unmarshal(body, out) != nil {
					t.Fatal("invalid command response")
				}
			}
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "Private retention fixture"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "fixture", "url": "https://github.com/fixture/retention"}}, {Action: "create_application", ID: "a", ProductID: "p", Data: map[string]any{"name": "component", "repository_id": "r"}}, {Action: "configure_artifact_retention", ProductID: "p", Data: map[string]any{"application_id": "a", "keep_last": 3, "keep_current": false, "keep_previous": 0}}} {
				post(c, 200, nil)
			}
			if status, _, _ := request(server.URL, "GET", "/api/products/p/retention", nil, false, nil); status != 401 {
				t.Fatal("inventory API lacks protection")
			}
			command := domain.Command{Action: "pin_artifact", ProductID: "p", Data: map[string]any{"application_id": "a", "image_repository": "REGISTRY.example:443/team/image", "digest": "sha256:" + strings.Repeat("a", 64), "purpose": "manual", "request_key": "protected-pin-first"}}
			var pin domain.ArtifactPin
			post(command, 200, &pin)
			if pin.ImageRepository != "registry.example/team/image" {
				t.Fatal("pin not normalized")
			}
			before, _ := store.Read(ctx)
			post(domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Private retention fixture"}}, 422, nil)
			post(command, 200, nil)
			after, _ := store.Read(ctx)
			if len(before.Events) != len(after.Events) || !reflect.DeepEqual(before.ArtifactPins, after.ArtifactPins) {
				t.Fatal("pin retry changed state")
			}
			conflict := command
			conflict.Data = map[string]any{}
			for k, v := range command.Data {
				conflict.Data[k] = v
			}
			conflict.Data["purpose"] = "rollback"
			post(conflict, 422, nil)
			post(domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: pin.ID, Data: map[string]any{"application_id": "wrong"}}, 422, nil)
			query := func() domain.RetentionReport {
				t.Helper()
				status, b, _ := request(server.URL, "GET", "/api/products/p/retention", nil, true, nil)
				if status != 200 {
					t.Fatalf("retention query:%d %s", status, b)
				}
				var report domain.RetentionReport
				if e := json.Unmarshal(b, &report); e != nil {
					t.Fatal(e)
				}
				return report
			}
			report := query()
			if report.DeletionAllowed || len(report.Images) != 1 || !report.Images[0].Protected || len(report.Policies) != 1 || report.Policies[0].ConfiguredKeepLast != 3 || report.Policies[0].EffectiveKeepLast != 10 {
				t.Fatalf("typed floor/pin report:%+v", report)
			}
			direct := domain.EvaluateRetention(after, "p", report.EvaluatedAt, nil)
			if !reflect.DeepEqual(report, direct) {
				t.Fatal("HTTP diverged from domain report")
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "retention-fixture", Version: "1"}, nil)
			session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: retentionAuthTransport{token}}, DisableStandaloneSSE: true}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_artifact_retention", Arguments: map[string]any{"product_id": "p"}})
			if e != nil || result.IsError {
				t.Fatalf("MCP inventory:%v %+v", e, result)
			}
			b, _ := json.Marshal(result.StructuredContent)
			var mcpReport domain.RetentionReport
			if e = json.Unmarshal(b, &mcpReport); e != nil {
				t.Fatal(e)
			}
			if mcpReport.InputHash != report.InputHash || mcpReport.DeletionAllowed {
				t.Fatal("MCP and HTTP evidence disagree")
			}
			status, b, _ := request(server.URL, "GET", "/api/attention?product_id=p", nil, true, nil)
			if status != 200 || !bytes.Contains(b, []byte(report.InputHash)) {
				t.Fatalf("attention not from same report:%d %s", status, b)
			}
			afterQueries, _ := store.Read(ctx)
			if len(afterQueries.Events) != len(after.Events) {
				t.Fatal("pure inventory/attention queries wrote audit")
			}
			// Reopen the real local/PG store, retaining typed references/fingerprint.
			session.Close()
			server.Close()
			store.Close()
			store = open()
			service = application.NewWithProvider(store, &retentionNoIOProvider{})
			server = httptest.NewServer(transport.New(service, transport.Options{Token: token}))
			defer server.Close()
			if reopened := query(); reopened.InputHash != report.InputHash {
				t.Fatal("reopen changed same evidence identity")
			}
			release := domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: pin.ID, Data: map[string]any{"application_id": "a"}}
			post(release, 200, nil)
			released, _ := store.Read(ctx)
			post(release, 200, nil)
			post(command, 200, nil)
			retried, _ := store.Read(ctx)
			if len(released.Events) != len(retried.Events) || retried.ArtifactPins[0].ReleasedAt == nil {
				t.Fatal("released create retry reactivated pin or appended audit")
			}
			active := command
			active.Data = map[string]any{}
			for k, v := range command.Data {
				active.Data[k] = v
			}
			active.Data["request_key"] = "protected-pin-active"
			active.Data["digest"] = "sha256:" + strings.Repeat("b", 64)
			post(active, 200, nil)
			if e := store.Update(ctx, func(st *domain.State) error { st.ArtifactPins[0].RequestHash = "corrupt"; return nil }); e == nil {
				t.Fatal("store accepted corrupt pin fingerprint")
			}
			if e := store.Update(ctx, func(st *domain.State) error {
				p := &st.ArtifactPins[1]
				p.ReleasedAt = &p.UpdatedAt
				return nil
			}); e == nil {
				t.Fatal("store accepted release without explicit command event")
			}
			status, archive, headers := request(server.URL, "POST", "/api/backups/export", nil, true, nil)
			if status != 200 {
				t.Fatal("backup export rejected")
			}
			var destination retentionTestStore
			if kind == "local" {
				destination, e = persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "restored.db"))
			} else {
				destination, e = persistence.Open(ctx, isolatedDatabase(t))
			}
			if e != nil {
				t.Fatal(e)
			}
			defer destination.Close()
			restoredService := application.NewWithProvider(destination, &retentionNoIOProvider{})
			restored := httptest.NewServer(transport.New(restoredService, transport.Options{Token: token}))
			defer restored.Close()
			zr, e := gzip.NewReader(bytes.NewReader(archive))
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := io.ReadAll(zr)
			zr.Close()
			for _, corruption := range []string{"request_hash", "release_without_event"} {
				var envelope map[string]any
				json.Unmarshal(raw, &envelope)
				pins := envelope["state"].(map[string]any)["artifact_pins"].([]any)
				if corruption == "request_hash" {
					pins[0].(map[string]any)["request_hash"] = "corrupt"
				} else {
					pin := pins[1].(map[string]any)
					pin["released_at"] = pin["updated_at"]
				}
				mutated, _ := json.Marshal(envelope)
				var buf bytes.Buffer
				zw := gzip.NewWriter(&buf)
				zw.Write(mutated)
				zw.Close()
				bad := buf.Bytes()
				badHash := fmt.Sprintf("%x", sha256.Sum256(bad))
				status, _, _ = request(restored.URL, "POST", "/api/backups/restore", bad, true, map[string]string{"Content-Type": "application/gzip", "X-Backup-SHA256": badHash})
				if status != 422 {
					t.Fatalf("restore accepted pin corruption %s", corruption)
				}
			}
			status, b, _ = request(restored.URL, "POST", "/api/backups/restore", archive, true, map[string]string{"Content-Type": "application/gzip", "X-Backup-SHA256": headers.Get("X-Backup-SHA256")})
			if status != 200 {
				t.Fatalf("valid restore:%d %s", status, b)
			}
			imported, _ := destination.Read(ctx)
			if len(imported.ArtifactPins) != 2 || imported.ArtifactPins[0].ReleasedAt == nil || imported.ArtifactPins[1].ReleasedAt != nil || len(imported.NativeQuarantines) != 1 {
				t.Fatal("restore lost pin lifecycle or quarantine")
			}
			inventory, e := restoredService.Query(ctx, "get_artifact_retention", "p")
			if e != nil {
				t.Fatal(e)
			}
			restoredReport := inventory.(domain.RetentionReport)
			if restoredReport.DeletionAllowed || len(restoredReport.Images) == 0 || !restoredReport.Images[0].Protected || !strings.Contains(strings.ToLower(strings.Join(restoredReport.Blockers, " ")), "quarantin") {
				t.Fatalf("restored inventory not conservative:%+v", restoredReport)
			}
			// Create and query through MCP use precisely the same command/report service.
			viaMCP := active
			viaMCP.Data = map[string]any{}
			for k, v := range active.Data {
				viaMCP.Data[k] = v
			}
			viaMCP.Data["request_key"] = "mcp-pin-request"
			viaMCP.Actor = "fixture/operator"
			second := mcp.NewClient(&mcp.Implementation{Name: "retention-command-fixture", Version: "1"}, nil)
			s2, e := second.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: retentionAuthTransport{token}}, DisableStandaloneSSE: true}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer s2.Close()
			executed, e := s2.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: viaMCP})
			if e != nil || executed.IsError {
				t.Fatalf("MCP pin command:%v %+v", e, executed)
			}
			current, _ := store.Read(ctx)
			for _, p := range current.ArtifactPins {
				if p.ReleasedAt == nil {
					post(domain.Command{Action: "release_artifact_pin", ProductID: "p", ID: p.ID, Data: map[string]any{"application_id": "a"}}, 200, nil)
				}
			}
			beforeDeletion, _ := store.Read(ctx)
			post(domain.Command{Action: "delete_product", ProductID: "p", Data: map[string]any{"name": "Private retention fixture"}}, 200, nil)
			deleted, _ := store.Read(ctx)
			if len(deleted.ArtifactPins) != 0 || len(deleted.Applications) != 0 || len(deleted.Events) != len(beforeDeletion.Events)+1 {
				t.Fatal("explicit released-pin product deletion lost provenance or left dangling refs")
			}
		})
	}
}

func TestRetentionPinCorruptionFailsClosedOnStoredReadAndReopen(t *testing.T) {
	for _, fixture := range []struct{ kind, mutation string }{{"local", "request_hash"}, {"postgres", "request_hash"}, {"local", "release_without_event"}, {"postgres", "release_without_event"}} {
		kind := fixture.kind
		t.Run(kind+"/"+fixture.mutation, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "corrupt-pins.db")
			db := ""
			if kind == "postgres" {
				db = isolatedDatabase(t)
			}
			var store retentionTestStore
			var err error
			if kind == "local" {
				store, err = persistence.OpenLocal(ctx, path)
			} else {
				store, err = persistence.Open(ctx, db)
			}
			if err != nil {
				t.Fatal(err)
			}
			service := application.New(store)
			for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "corruption fixture"}}, {Action: "create_repository", ID: "r", ProductID: "p", Data: map[string]any{"name": "fixture", "url": "https://github.com/fixture/corruption"}}, {Action: "create_application", ID: "a", ProductID: "p", Data: map[string]any{"name": "component", "repository_id": "r"}}, {Action: "pin_artifact", ProductID: "p", Data: map[string]any{"application_id": "a", "image_repository": "registry.example/image", "digest": "sha256:" + strings.Repeat("a", 64), "purpose": "manual", "request_key": "corruption-test"}}} {
				c.Actor = "fixture/operator"
				if _, err = service.Execute(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			state, _ := store.Read(ctx)
			id := state.ArtifactPins[0].ID
			store.Close()
			if kind == "local" {
				raw, e := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
				if e != nil {
					t.Fatal(e)
				}
				e = raw.Update(func(tx *bolt.Tx) error {
					bucket := tx.Bucket([]byte("release-control"))
					var st domain.State
					if err := json.Unmarshal(bucket.Get([]byte("state")), &st); err != nil {
						return err
					}
					if fixture.mutation == "request_hash" {
						st.ArtifactPins[0].RequestHash = "corrupt"
					} else {
						st.ArtifactPins[0].ReleasedAt = &st.ArtifactPins[0].UpdatedAt
					}
					b, _ := json.Marshal(st)
					return bucket.Put([]byte("state"), b)
				})
				raw.Close()
				if e != nil {
					t.Fatal(e)
				}
				reopened, e := persistence.OpenLocal(ctx, path)
				if e == nil {
					reopened.Close()
					t.Fatal("local reopen accepted corrupt pin")
				}
			} else {
				conn, e := pgx.Connect(ctx, db)
				if e != nil {
					t.Fatal(e)
				}
				if fixture.mutation == "request_hash" {
					_, e = conn.Exec(ctx, "UPDATE records SET document=jsonb_set(document,'{request_hash}',to_jsonb('corrupt'::text)) WHERE kind='artifact_pins' AND id=$1", id)
				} else {
					_, e = conn.Exec(ctx, "UPDATE records SET document=jsonb_set(document,'{released_at}',document->'updated_at') WHERE kind='artifact_pins' AND id=$1", id)
				}
				conn.Close(ctx)
				if e != nil {
					t.Fatal(e)
				}
				reopened, e := persistence.Open(ctx, db)
				if e != nil {
					t.Fatal(e)
				}
				defer reopened.Close()
				if _, e = reopened.Read(ctx); e == nil {
					t.Fatal("PG read/reopen accepted corrupt pin")
				}
				if _, e = application.New(reopened).Query(ctx, "get_artifact_retention", "p"); e == nil {
					t.Fatal("PG inventory consumed corrupt pin")
				}
			}
		})
	}
}
