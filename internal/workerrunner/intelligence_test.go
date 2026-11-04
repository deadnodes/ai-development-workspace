package workerrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/intelligencepatch"
	"releasecontrol/internal/testdocker"
	"releasecontrol/internal/workerconfig"
)

func patchExecutionFixture(t *testing.T) (Config, string, *journalState, *intelligencepatch.Proposal) {
	t.Helper()
	git := newSourceGitFixture(t)
	baseSHA := string(git.git("rev-parse", "base"))
	cfg := sourceFetchConfig(t)
	cfg.Recipe.Execution.SourceFetch = nil
	cfg.SourceGitHub = nil
	cfg.SourceMirror = filepath.Join(git.root, ".git")
	cfg.Recipe.IntelligencePaths = []string{"a.go", "b.go"}
	cfg.Recipe.Execution.CheckArgv = []string{"go", "test", "/source/main.go", "/source/a.go", "/source/b.go", "/source/main_test.go"}
	cfg.Recipe.Hash = workerconfig.RecipeHash(cfg.Recipe)
	root := t.TempDir()
	owner := nonce()
	js := &journalState{path: filepath.Join(root, "journal.json"), value: journal{Owner: owner, ParentPID: os.Getpid(), SourceSHA: baseSHA, Deadline: time.Now().Add(time.Minute), LeaseUntil: time.Now().Add(time.Minute)}}
	proposal := &intelligencepatch.Proposal{BaseSHA: baseSHA, HeadSHA: baseSHA, Edits: []intelligencepatch.Edit{{Path: "a.go", BeforeHash: digestBytes([]byte("package main\nconst A=\"A0\"\n")), Content: "package main\nconst A=\"A1\"\n"}, {Path: "b.go", BeforeHash: digestBytes([]byte("package main\nconst B=\"B0\"\n")), Content: "package main\nconst B=\"B1\"\n"}}}
	return cfg, root, js, proposal
}

func TestIntelligencePatchProductionMaterializationPath(t *testing.T) {
	// A deliberately unavailable Docker runtime proves the production execute
	// branch applies only to its materialized snapshot before runtime admission;
	// it does not stand in for a passing isolated compiler check.
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "valid_preimage", false: "wrong_preimage"}[valid], func(t *testing.T) {
			cfg, root, js, p := patchExecutionFixture(t)
			bin := t.TempDir()
			if e := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if !valid {
				p.Edits[0].BeforeHash = "sha256:" + strings.Repeat("f", 64)
			}
			_, e := execute(context.Background(), cfg, root, js, nil, nil, p)
			if e == nil {
				t.Fatal("runtime failure expected")
			}
			content, readErr := os.ReadFile(filepath.Join(root, "source", "a.go"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := "A0"
			if valid {
				want = "A1"
			}
			if !strings.Contains(string(content), want) {
				t.Fatalf("wrong production patch behavior: %q", content)
			}
			info, _ := os.Stat(filepath.Join(root, "source", "a.go"))
			if info.Mode().Perm() != 0444 {
				t.Fatal("read-only source mode changed")
			}
			if !valid && !strings.Contains(e.Error(), "bounded patch application failed") {
				t.Fatalf("invalid proposal reached runtime: %v", e)
			}
		})
	}
}

func TestIntelligencePatchRealIsolatedCheck(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" {
		t.Skip("explicit owned Docker fixture opt-in required")
	}
	d := testdocker.Load(t)
	cfg, root, js, p := patchExecutionFixture(t)
	cfg.DockerContext = d.Context
	home, e := os.UserHomeDir()
	if e != nil {
		t.Fatal(e)
	}
	cfg.DockerConfig = filepath.Join(home, ".docker")
	cfg.Recipe.Execution.Platform = d.Platform
	cfg.Recipe.Execution.ToolchainImage = "golang@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b"
	cfg.Recipe.Execution.MemoryMB = 1536
	cfg.Recipe.Execution.StorageMB = 512
	cfg.Recipe.Execution.PIDs = 128
	cfg.Recipe.Execution.LogBytes = 1 << 20
	cfg.Recipe.Execution.TimeoutSeconds = 180
	cfg.Recipe.Hash = workerconfig.RecipeHash(cfg.Recipe)
	js.value.DockerContext = cfg.DockerContext
	js.value.DockerConfig = cfg.DockerConfig
	js.value.Deadline = time.Now().Add(3 * time.Minute)
	js.value.LeaseUntil = js.value.Deadline
	if e = saveJournal(js.path, js.value); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := cleanup(js.path); e != nil {
			t.Error(e)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, e := execute(ctx, cfg, root, js, nil, nil, p)
	if e != nil || result != nil {
		t.Fatalf("real isolated source check failed: %v result%+v", e, result)
	}
	// The existing fixture's TestBoundaries checks both A1+B1, no credentials,
	// no Git/socket mounts, read-only source and disabled network in the actual
	// trusted toolchain container. Source-check produces no OCI publication.
}
