package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeReporterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reporter-token")
	if err := os.WriteFile(path, []byte(" reporter-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCP_RUNTIME_REPORTER_TOKEN_FILE", path)
	for _, tc := range []struct {
		name, scope string
		wantErr     bool
	}{
		{"one scope", "p:prod:cluster", false},
		{"empty grants nothing", "", false},
		{"second cluster", "p:prod:cluster,p:prod:other", true},
		{"second product", "p:prod:cluster,q:prod:cluster", true},
		{"second environment", "p:prod:cluster,p:dev:cluster", true},
		{"duplicate entry", "p:prod:cluster, p:prod:cluster", true},
		{"missing cluster", "p:prod", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RCP_RUNTIME_REPORTER_SCOPE", tc.scope)
			token, err := runtimeReporterConfig()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "RCP_RUNTIME_REPORTER_SCOPE") {
					t.Fatalf("want scope configuration error, got %v", err)
				}
				if strings.Contains(tc.scope, ",") && !strings.Contains(err.Error(), "per-scope tokens") {
					t.Fatalf("missing actionable token error: %v", err)
				}
				if token != "" {
					t.Fatal("invalid configuration returned a token")
				}
			} else if err != nil || token != "reporter-token" {
				t.Fatalf("token=%q err=%v", token, err)
			}
		})
	}
}
