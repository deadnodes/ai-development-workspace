package transport_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/transport"
)

// Stub success proves serialization only; lifecycle acceptance belongs to the
// application's restoration and actual pipeline tests.
func TestDevRestorationTransportParityAndStrictInputs(t *testing.T) {
	service := &boundaryService{commands: make(chan domain.Command, 32), queries: make(chan [2]string, 1)}
	server := httptest.NewServer(transport.New(service, transport.Options{}))
	defer server.Close()
	ctx := context.Background()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "restore-boundary", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct{ action, path, fields string }{
		{"request_candidate_dev_restoration", "request-restoration", `"reservation_operation_id":"hold"`},
		{"confirm_candidate_dev_restoration", "confirm-restoration", `"restoration_operation_id":"restore","expected_plan_hash":"exact-plan"`},
		{"verify_candidate_dev_restoration", "verify-restoration", `"restoration_operation_id":"restore"`},
		{"release_candidate_dev_after_restoration", "release-after-restoration", `"restoration_operation_id":"restore","restoration_verification_operation_id":"proof"`},
	} {
		t.Run(tc.action, func(t *testing.T) {
			defer func() {
				for len(service.commands) > 0 {
					<-service.commands
				}
			}()
			data := `{` + tc.fields + `,"request_key":"restoration-request-0001","expected_selection":{"composition_id":"baseline","operation_id":"old","generation":9007199254740993}}`
			flat := `{"actor":"agent/owner","product_id":"product",` + data[1:]
			generic := `{"action":"` + tc.action + `","actor":"agent/owner","product_id":"product","data":` + data + `}`
			post := func(path, body string, want int) {
				t.Helper()
				r, e := http.Post(server.URL+path, "application/json", strings.NewReader(body))
				if e != nil {
					t.Fatal(e)
				}
				r.Body.Close()
				if r.StatusCode != want {
					t.Fatalf("%s body=%s HTTP%d want%d", path, body, r.StatusCode, want)
				}
			}
			post("/api/products/product/candidate-dev/"+tc.path, flat, 200)
			named := <-service.commands
			post("/api/commands", generic, 200)
			viaGeneric := <-service.commands
			if !reflect.DeepEqual(named, viaGeneric) {
				t.Fatalf("HTTP commands differ: %+v %+v", named, viaGeneric)
			}
			for _, call := range []struct{ name, raw string }{{tc.action, flat}, {"execute", generic}} {
				result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: json.RawMessage(call.raw)})
				if e != nil || result.IsError {
					t.Fatalf("SDK %s: %v %+v", call.name, e, result)
				}
				command := <-service.commands
				if !reflect.DeepEqual(named, command) {
					t.Fatalf("SDK lost exact command: %+v", command)
				}
			}
			selection, _ := json.Marshal(named.Data["expected_selection"])
			if !strings.Contains(string(selection), "9007199254740993") {
				t.Fatalf("rounded CAS: %s", selection)
			}
			for _, bad := range []string{
				strings.Replace(flat, `"actor":"agent/owner",`, "", 1),
				strings.Replace(flat, `"agent/owner"`, `"   "`, 1),
				strings.Replace(flat, `"product"`, `"foreign"`, 1),
				strings.Replace(flat, `"actor"`, `"Actor"`, 1),
				strings.Replace(flat, `"request_key":`, `"request_key":"duplicate-request-key","request_key":`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":1,"generation":2`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"Generation":1`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":null`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":-1`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":1.5`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":18446744073709551616`, 1),
				strings.Replace(flat, `"generation":9007199254740993`, `"generation":1,"clock":"now"`, 1),
				strings.Replace(flat, `"request_key":"restoration-request-0001"`, `"request_key":"short"`, 1),
				strings.Replace(flat, `"actor":`, `"approved":true,"actor":`, 1),
				flat + ` {}`, `null`,
			} {
				post("/api/products/product/candidate-dev/"+tc.path, bad, 400)
			}
			for _, bad := range []string{
				strings.Replace(generic, `"actor":"agent/owner",`, "", 1),
				strings.Replace(generic, `"agent/owner"`, `" "`, 1),
				strings.Replace(generic, `"data":`, `"feature_id":"foreign","data":`, 1),
				strings.Replace(generic, `"data":`, `"id":"","data":`, 1),
				strings.Replace(generic, `"request_key":`, `"request_key":"duplicate-request-key","request_key":`, 1),
				strings.Replace(generic, `"generation":9007199254740993`, `"Generation":1`, 1),
				strings.Replace(generic, `"data":{`, `"data":{"trust_grade":"HUMAN",`, 1),
				generic + ` {}`,
			} {
				post("/api/commands", bad, 400)
			}
			for _, call := range []struct{ name, raw string }{
				{tc.action, strings.Replace(flat, `"actor":"agent/owner",`, "", 1)},
				{tc.action, strings.Replace(flat, `"agent/owner"`, `" "`, 1)},
				{tc.action, strings.Replace(flat, `"generation":9007199254740993`, `"generation":1,"other":2`, 1)},
				{tc.action, strings.Replace(flat, `"actor":`, `"healthy":true,"actor":`, 1)},
				{tc.action, strings.Replace(flat, `"request_key":`, `"request_key":"duplicate-request-key","request_key":`, 1)},
				{"execute", strings.Replace(generic, `"actor":"agent/owner",`, "", 1)},
				{"execute", strings.Replace(generic, `"agent/owner"`, `" "`, 1)},
				{"execute", strings.Replace(generic, `"data":`, `"feature_id":"foreign","data":`, 1)},
				{"execute", strings.Replace(generic, `"data":{`, `"data":{"approved":true,`, 1)},
			} {
				result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: json.RawMessage(call.raw)})
				if e == nil && !result.IsError {
					t.Fatalf("SDK accepted invalid %s: %s", call.name, call.raw)
				}
			}
			if len(service.commands) != 0 {
				t.Fatalf("invalid restoration input reached application: %d", len(service.commands))
			}
		})
	}
	// Existing generic commands keep their historical attribution default.
	response, err := http.Post(server.URL+"/api/commands", "application/json", strings.NewReader(`{"action":"create_product","data":{"name":"legacy"}}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || (<-service.commands).Actor != "agent" {
		t.Fatal("legacy actor default changed")
	}
}
