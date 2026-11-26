package transport

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http/httptest"
	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
)

const maxIdenticalErrorGroup = 5

func TestAgentGuideNoBoilerplateErrors(t *testing.T) {
	groups := map[string][]string{}
	for _, entry := range agentguide.Registry() {
		encoded, err := json.Marshal(entry.Errors)
		if err != nil {
			t.Fatalf("encode %s errors: %v", entry.Name, err)
		}
		groups[string(encoded)] = append(groups[string(encoded)], entry.Kind+":"+entry.Name)
	}
	for _, entries := range groups {
		if len(entries) > maxIdenticalErrorGroup {
			t.Errorf("%d entries share an identical Errors block (maximum %d): %v", len(entries), maxIdenticalErrorGroup, entries)
		}
	}
}

func TestAgentGuideEntriesHaveCompleteBriefs(t *testing.T) {
	entries := agentguide.Registry()
	if pending := agentguide.PendingDocumentation(); len(pending) != 0 {
		t.Errorf("pending documentation remains: %v", pending)
	}
	if len(entries) == 0 {
		t.Fatal("empty guide registry")
	}
	for _, e := range entries {
		if strings.TrimSpace(e.Purpose) == "" || strings.TrimSpace(e.When) == "" || strings.TrimSpace(e.Output) == "" || strings.TrimSpace(e.Authority) == "" || strings.TrimSpace(e.Example) == "" || len(e.Errors) == 0 {
			t.Errorf("%s has an empty required brief section", e.Kind+":"+e.Name)
		}
		if len(e.Inputs) == 0 {
			t.Errorf("%s must document inputs or an explicit no-inputs field", e.Kind+":"+e.Name)
		}
		for _, f := range e.Inputs {
			if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Type) == "" || strings.TrimSpace(f.Meaning) == "" {
				t.Errorf("%s has incomplete input field: %+v", e.Name, f)
			}
		}
		for _, er := range e.Errors {
			if strings.TrimSpace(er.Code) == "" || strings.TrimSpace(er.Meaning) == "" {
				t.Errorf("%s has incomplete error", e.Name)
			}
		}
	}
}

func TestAgentGuideExecuteExamplesMatchCommandSchema(t *testing.T) {
	entries := agentguide.Registry()
	root := CommandSchema()
	rootRaw, _ := json.Marshal(root)
	var rootSchema jsonschema.Schema
	if err := json.Unmarshal(rootRaw, &rootSchema); err != nil {
		t.Fatal(err)
	}
	fullSchema, err := rootSchema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	variants := root["oneOf"].([]any)
	branches := map[string]map[string]any{}
	for _, raw := range variants {
		b := raw.(map[string]any)
		props := b["properties"].(map[string]any)
		name := props["action"].(map[string]any)["const"].(string)
		branches[name] = b
	}
	for _, e := range entries {
		if e.Kind != "execute_action" {
			continue
		}
		if e.Area == "execution" { // These actions are intentionally exposed as named MCP tools.
			continue
		}
		var example map[string]any
		if err := json.Unmarshal([]byte(e.Example), &example); err != nil {
			t.Errorf("%s example is not JSON: %v", e.Name, err)
			continue
		}
		schemaBranch, ok := branches[e.Name]
		if !ok {
			t.Errorf("%s has no command-schema branch", e.Name)
			continue
		}
		raw, _ := json.Marshal(schemaBranch)
		var sch jsonschema.Schema
		if err := json.Unmarshal(raw, &sch); err != nil {
			t.Errorf("%s schema decode: %v", e.Name, err)
			continue
		}
		resolved, err := sch.Resolve(nil)
		if err != nil {
			t.Errorf("%s schema resolve: %v", e.Name, err)
			continue
		}
		if err = resolved.Validate(example); err != nil {
			t.Errorf("%s example does not validate: %v", e.Name, err)
		}
		// MCP validates against the whole schema, including the root data allow-list.
		if err = fullSchema.Validate(example); err != nil {
			t.Errorf("%s example does not validate against the full command schema: %v", e.Name, err)
		}
		// Every documented top-level command field must be present in this action branch.
		props := schemaBranch["properties"].(map[string]any)
		documented := map[string]bool{}
		for _, f := range e.Inputs {
			documented[f.Name] = true
		}
		requiredFields := []string{}
		switch fields := schemaBranch["required"].(type) {
		case []string:
			requiredFields = fields
		case []any:
			for _, field := range fields {
				requiredFields = append(requiredFields, field.(string))
			}
		}
		for _, name := range requiredFields {
			if name != "action" && name != "data" && !documented[name] {
				t.Errorf("%s omits required schema field %q", e.Name, name)
			}
		}
		if dr, ok := props["data"].(map[string]any); ok {
			if req, ok := dr["required"].([]any); ok {
				for _, raw := range req {
					name := raw.(string)
					if !documented[name] {
						t.Errorf("%s omits required data field %q", e.Name, name)
					}
				}
			}
		}
		rootProps := root["properties"].(map[string]any)
		// Copy: the shared root data properties must not accumulate other actions' fields.
		dataProps := map[string]any{}
		for k, v := range root["properties"].(map[string]any)["data"].(map[string]any)["properties"].(map[string]any) {
			dataProps[k] = v
		}
		if dr, ok := props["data"].(map[string]any); ok {
			if dp, ok := dr["properties"].(map[string]any); ok {
				for k, v := range dp {
					dataProps[k] = v
				}
			}
			// A field the action's branch requires is part of its contract even when
			// the shared property map does not describe it.
			switch req := dr["required"].(type) {
			case []string:
				for _, name := range req {
					dataProps[name] = true
				}
			case []any:
				for _, raw := range req {
					dataProps[raw.(string)] = true
				}
			}
		}
		for _, f := range e.Inputs {
			if f.Name == "data" || f.Name == "actor" {
				continue
			}
			if _, ok := props[f.Name]; ok {
				continue
			}
			if _, ok := rootProps[f.Name]; ok {
				continue
			}
			if _, ok := dataProps[f.Name]; !ok {
				t.Errorf("%s documents unknown command field %q", e.Name, f.Name)
			}
		}
	}
}

func TestAgentGuideMCPExamplesMatchRegisteredToolSchemas(t *testing.T) {
	ctx := context.Background()
	store, err := persistence.OpenSQLite(ctx, filepath.Join(t.TempDir(), "guide.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	httpServer := httptest.NewServer(New(application.New(store), Options{}))
	defer httpServer.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "agent-guide-quality", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]any{}
	for _, tool := range listed.Tools {
		actual[tool.Name] = tool.InputSchema
	}
	for _, e := range agentguide.Registry() {
		if e.Kind != "mcp_tool" {
			continue
		}
		schema, ok := actual[e.Name]
		if !ok {
			t.Errorf("%s has no registered MCP tool", e.Name)
			continue
		}
		var example any
		if err := json.Unmarshal([]byte(e.Example), &example); err != nil {
			t.Errorf("%s example is not JSON: %v", e.Name, err)
			continue
		}
		if wrapper, ok := example.(map[string]any); ok && wrapper["name"] == e.Name {
			example = wrapper["arguments"]
		}
		if e.Name == "execute" {
			continue
		} // Command action examples are checked against /api/schema above.
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		var sch jsonschema.Schema
		if err = json.Unmarshal(raw, &sch); err != nil {
			t.Errorf("%s schema decode: %v", e.Name, err)
			continue
		}
		resolved, err := sch.Resolve(nil)
		if err != nil {
			t.Errorf("%s schema resolve: %v", e.Name, err)
			continue
		}
		if err = resolved.Validate(example); err != nil {
			t.Errorf("%s example does not validate against registered MCP schema: %v", e.Name, err)
		}
	}
}
