package transport

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"releasecontrol/internal/agentguide"
	"releasecontrol/internal/domain"
)

// This intentionally inspects the same registrations used to construct New:
// static MCP AddTool names, the named flow tools and execution command tools,
// ServeMux patterns, and the authoritative domain action catalog.
func TestAgentGuideRegistryDrift(t *testing.T) {
	known := map[string]bool{}
	for _, e := range agentguide.Registry() {
		known[e.Kind+":"+e.Name] = true
		for _, alias := range e.Aliases {
			if known[alias] {
				t.Errorf("duplicate registry alias %q", alias)
			}
			known[alias] = true
		}
	}
	for _, p := range agentguide.PendingDocumentation() {
		if known[p] {
			t.Fatalf("%s is both documented and pending", p)
		}
		known[p] = true
	}
	toolName := regexp.MustCompile(`Name:\s*"([a-z][a-z0-9_]+)"`)
	route := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ /[^" ]+)"`)
	pattern := regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) /[^" ]+)"`)
	barePattern := regexp.MustCompile(`\.Handle\("(/[^" ]+)"`)
	tools, routes := map[string]bool{}, map[string]bool{}
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(".", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range toolName.FindAllSubmatch(b, -1) {
			tools[string(m[1])] = true
		}
		for _, m := range route.FindAllSubmatch(b, -1) {
			routes[string(m[1])] = true
		}
		for _, m := range pattern.FindAllSubmatch(b, -1) {
			routes[string(m[1])] = true
		}
		for _, m := range barePattern.FindAllSubmatch(b, -1) {
			routes[string(m[1])] = true
		}
	}
	flow, err := os.ReadFile("flow.go")
	if err != nil {
		t.Fatal(err)
	}
	flowName := regexp.MustCompile(`\{"([a-z_]+)",\s*"POST /`)
	for _, m := range flowName.FindAllSubmatch(flow, -1) {
		tools[string(m[1])] = true
	}
	external, err := os.ReadFile("external.go")
	if err != nil {
		t.Fatal(err)
	}
	execNames := regexp.MustCompile(`for _, action := range \[\]string\{([^}]+)\}`).FindSubmatch(external)
	if len(execNames) > 1 {
		for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(execNames[1], -1) {
			tools[string(m[1])] = true
		}
	}
	for n := range tools {
		if !known["mcp_tool:"+n] {
			t.Errorf("MCP tool %q is neither documented nor pending", n)
		}
		delete(known, "mcp_tool:"+n)
	}
	for n := range routes {
		if !known["http_route:"+n] {
			t.Errorf("HTTP route %q is neither documented nor pending", n)
		}
		delete(known, "http_route:"+n)
	}
	for _, n := range domain.Actions {
		if !known["execute_action:"+n] {
			t.Errorf("execute action %q is neither documented nor pending", n)
		}
		delete(known, "execute_action:"+n)
	}
	for key := range known {
		t.Errorf("registry names non-existent interface %q", key)
	}
}
