package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func TestFeaturePatchHTTPMCPPersistence(t *testing.T) {
	for _, backend := range []string{"local", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			var store application.Store
			var closeStore func()
			var reopen func()
			if backend == "postgres" {
				dsn := isolatedDatabase(t)
				reopen = func() {
					s, e := persistence.Open(ctx, dsn)
					if e != nil {
						t.Fatal(e)
					}
					store = s
					closeStore = s.Close
				}
			} else {
				path := filepath.Join(t.TempDir(), "state.db")
				reopen = func() {
					s, e := persistence.OpenLocal(ctx, path)
					if e != nil {
						t.Fatal(e)
					}
					store = s
					closeStore = s.Close
				}
			}
			reopen()
			defer func() { closeStore() }()
			provider := &persistedDeliveryFixture{}
			service := application.NewWithProvider(store, provider)
			command := func(c domain.Command) any {
				t.Helper()
				c.Actor = "agent/feature-patch"
				v, e := service.Execute(ctx, c)
				if e != nil {
					t.Fatalf("%s: %v", c.Action, e)
				}
				return v
			}
			command(domain.Command{Action: "create_product", ID: "p", Data: map[string]any{"name": "P"}})
			command(domain.Command{Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "Fixture", "app_id": 1, "installation_id": 2, "owner": "team", "private_key_ref": "env:RCP_TEST_KEY"}})
			if _, e := service.Query(ctx, "discover_repositories", "conn"); e != nil {
				t.Fatal(e)
			}
			for _, repo := range []struct{ id, role string }{{"source", "APPLICATION"}, {"gitops", "GITOPS"}} {
				command(domain.Command{Action: "import_repository", ID: repo.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": "team/" + repo.id, "role": repo.role, "default_branch": "main"}})
			}
			command(domain.Command{Action: "create_application", ID: "app", ProductID: "p", Data: map[string]any{"name": "API", "repository_id": "source"}})
			command(domain.Command{Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "Dev", "cluster": "local", "namespace": "dev", "runtime": map[string]any{"marker": "observed"}}})
			command(domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "app", "connection_id": "conn", "workflow": "build.yml", "image_repository": "ghcr.io/team/api", "rebuild_missing": true}})
			command(domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "app", "connection_id": "conn", "repository_id": "gitops", "purpose": "DEV", "ref": "main", "path": "app.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}})
			for _, id := range []string{"a", "b"} {
				command(domain.Command{Action: "create_feature", ID: "f" + id, ProductID: "p", Data: map[string]any{"title": id, "goal": id}})
				command(domain.Command{Action: "create_integration", ID: "i" + id, FeatureID: "f" + id, Data: map[string]any{"title": id, "objective": id, "repositories": []string{"source"}}})
				for n, ch := range []string{"b", "c", "d"} {
					rid := id + string(rune('1'+n))
					command(domain.Command{Action: "record_integration_revision", ID: rid, IntegrationID: "i" + id, Data: map[string]any{"repository_id": "source", "branch": "feature/" + id, "base_commit": deliveryBase, "head_commit": strings.Repeat(ch, 40), "commits": []string{strings.Repeat(ch, 40)}}})
				}
			}
			command(domain.Command{Action: "plan_composition", ID: "baseline", ProductID: "p", Data: map[string]any{"name": "A+B", "environment_id": "dev", "components": []domain.CompositionComponent{{ApplicationID: "app", BaseRef: "main", BaseCommit: deliveryBase, TargetBranch: "generated/baseline", RevisionIDs: []string{"a1", "b1"}}}}})
			command(domain.Command{Action: "select_composition", ID: "baseline"})
			// Exercise exact integer transport beyond float64's precision boundary.
			const generation uint64 = 9007199254740993
			if e := store.Update(ctx, func(st *domain.State) error { st.Environments[0].SelectionGeneration = generation; return nil }); e != nil {
				t.Fatal(e)
			}
			server := httptest.NewServer(transport.New(service, transport.Options{}))
			defer func() { server.Close() }()
			session, e := mcp.NewClient(&mcp.Implementation{Name: "patch", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer session.Close()
			state := func() domain.State {
				t.Helper()
				st, e := store.Read(ctx)
				if e != nil {
					t.Fatal(e)
				}
				return st
			}
			fingerprint := func() string {
				b, _ := json.Marshal(state())
				var v any
				_ = json.Unmarshal(b, &v)
				b, _ = json.Marshal(v)
				return string(b)
			}
			patch := func(feature, rev, key string) domain.Command {
				st := state()
				env := st.Environments[0]
				return domain.Command{Action: "update_environment_feature", Actor: "agent/feature-patch", FeatureID: feature, Data: map[string]any{"environment_id": "dev", "revision_ids": []string{rev}, "idempotency_key": key, "expected_selection": domain.EnvironmentSelection{CompositionID: env.DesiredCompositionID, OperationID: env.DesiredOperationID, Generation: env.SelectionGeneration}}}
			}
			post := func(path string, value any, want int) []byte {
				t.Helper()
				b, _ := json.Marshal(value)
				r, e := http.Post(server.URL+path, "application/json", bytes.NewReader(b))
				if e != nil {
					t.Fatal(e)
				}
				defer r.Body.Close()
				out, _ := io.ReadAll(r.Body)
				if r.StatusCode != want {
					t.Fatalf("%s got %d want %d: %s", path, r.StatusCode, want, out)
				}
				return out
			}
			a := patch("fa", "a2", "http-a")
			var first domain.ExternalOperation
			if e = json.Unmarshal(post("/api/commands", a, 200), &first); e != nil {
				t.Fatal(e)
			}
			if first.FeatureUpdate.ExpectedSelection.Generation != generation {
				t.Fatal("HTTP rounded CAS token")
			}
			b := patch("fb", "b2", "mcp-b")
			args := map[string]any{"feature_id": b.FeatureID, "actor": b.Actor}
			for k, v := range b.Data {
				args[k] = v
			}
			result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_environment_feature", Arguments: args})
			if e != nil || result.IsError {
				t.Fatalf("named MCP: %v %+v", e, result)
			}
			st := state()
			comp := st.Compositions[len(st.Compositions)-1]
			if comp.AutoRefresh || !reflect.DeepEqual(comp.Components[0].RevisionIDs, []string{"a2", "b2"}) || comp.Components[0].BaseCommit != deliveryBase || st.Environments[0].SelectionGeneration != generation+2 {
				t.Fatalf("lost state %+v", st.Environments[0])
			}
			before := fingerprint()
			result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "execute", Arguments: a})
			if e != nil || result.IsError {
				t.Fatalf("generic MCP replay %v %+v content=%+v", e, result, result.Content[0])
			}
			post("/api/features/fb/update-environment", args, 200)
			if fingerprint() != before {
				t.Fatal("cross-transport replay mutated history")
			}
			// State queries expose exact generation independently of runtime evidence.
			resp, e := http.Get(server.URL + "/api/environments/dev/state")
			if e != nil {
				t.Fatal(e)
			}
			var query struct {
				Environment domain.Environment `json:"environment"`
			}
			e = json.NewDecoder(resp.Body).Decode(&query)
			resp.Body.Close()
			if e != nil || query.Environment.SelectionGeneration != generation+2 || query.Environment.Runtime["marker"] != "observed" {
				t.Fatal("state query lost desired/runtime separation")
			}
			result, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_environment_state", Arguments: map[string]any{"environment_id": "dev"}})
			if e != nil || result.IsError {
				t.Fatal("MCP state query failed")
			}
			// Concurrent identical first requests create one receipt, composition and event.
			c := patch("fa", "a3", "concurrent")
			oldOps, oldComps, oldEvents := len(st.Operations), len(st.Compositions), len(st.Events)
			var wg sync.WaitGroup
			failures := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() { defer wg.Done(); _, e := service.Execute(ctx, c); failures <- e }()
			}
			wg.Wait()
			close(failures)
			for e := range failures {
				if e != nil {
					t.Fatal(e)
				}
			}
			st = state()
			if len(st.Operations) != oldOps+1 || len(st.Compositions) != oldComps+1 || len(st.Events) != oldEvents+1 {
				t.Fatal("concurrent duplicate wrote extra records")
			}

			// Different feature updates from the same token: exactly one wins on
			// both durable backends, then an explicit reread/retry preserves both.
			left, right := patch("fa", "a1", "cas-a"), patch("fb", "b3", "cas-b")
			type attempt struct {
				command domain.Command
				err     error
			}
			attempts := make(chan attempt, 2)
			for _, request := range []domain.Command{left, right} {
				wg.Add(1)
				go func(request domain.Command) {
					defer wg.Done()
					_, err := service.Execute(ctx, request)
					attempts <- attempt{request, err}
				}(request)
			}
			wg.Wait()
			close(attempts)
			winners, losers := 0, 0
			var retry domain.Command
			prior := st
			for attempt := range attempts {
				if attempt.err == nil {
					winners++
				} else if errors.Is(attempt.err, application.ErrSelectionChanged) {
					losers++
					retry = attempt.command
				} else {
					t.Fatal(attempt.err)
				}
			}
			st = state()
			if winners != 1 || losers != 1 || len(st.Events) != len(prior.Events)+1 || len(st.Operations) != len(prior.Operations)+1 || len(st.Compositions) != len(prior.Compositions)+1 {
				t.Fatal("distinct CAS loser wrote records")
			}
			env := st.Environments[0]
			retry.Data["expected_selection"] = domain.EnvironmentSelection{CompositionID: env.DesiredCompositionID, OperationID: env.DesiredOperationID, Generation: env.SelectionGeneration}
			if _, err := service.Execute(ctx, retry); err != nil {
				t.Fatal(err)
			}
			st = state()
			latest := st.Compositions[len(st.Compositions)-1]
			if !reflect.DeepEqual(latest.Components[0].RevisionIDs, []string{"a1", "b3"}) {
				t.Fatal("explicit CAS retry dropped winning patch")
			}
			before = fingerprint()
			stale := b
			stale.Data["idempotency_key"] = "stale-new"
			post("/api/commands", stale, 422)
			if fingerprint() != before {
				t.Fatal("stale HTTP refusal wrote state")
			}
			// Reopen the actual backend and replay historical A2 after supersession.
			session.Close()
			server.Close()
			closeStore()
			reopen()
			service = application.NewWithProvider(store, provider)
			before = fingerprint()
			replay, e := service.Execute(ctx, a)
			if e != nil || replay.(domain.ExternalOperation).ID != first.ID || fingerprint() != before {
				t.Fatalf("reopen historical replay failed %v", e)
			}
			backup, e := service.CreateBackup(ctx, "tester")
			if e != nil {
				t.Fatal(e)
			}
			restored, e := persistence.OpenLocal(ctx, filepath.Join(t.TempDir(), "restored.db"))
			if e != nil {
				t.Fatal(e)
			}
			defer restored.Close()
			rs := application.New(restored)
			if _, e = rs.RestoreBackup(ctx, "tester", backup.ArchiveBase64, backup.SHA256); e != nil {
				t.Fatal(e)
			}
			restoredState, _ := restored.Read(ctx)
			events := len(restoredState.Events)
			replay, e = rs.Execute(ctx, a)
			if e != nil || replay.(domain.ExternalOperation).ID != first.ID {
				t.Fatalf("restore historical replay failed %v", e)
			}
			restoredState, _ = restored.Read(ctx)
			if len(restoredState.Events) != events || restoredState.Environments[0].SelectionGeneration != generation+5 {
				t.Fatal("restore replay changed selection/audit")
			}
			if provider.dispatches.Load() != 0 || provider.applies.Load() != 0 {
				t.Fatal("test unexpectedly performed provider mutations")
			}
		})
	}
}
