package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/artifactspool"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/publishrunner"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
)

type realApplicationProfile struct {
	Toolchain   string                     `json:"toolchain_image"`
	ProfileHash string                     `json:"profile_sha256"`
	Source      string                     `json:"source_sha"`
	BuildKit    string                     `json:"buildkit_image"`
	Runtime     domain.WorkerRuntimeRootFS `json:"runtime_rootfs"`
}

func TestRealApplicationOperatorProfileGuards(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("existing Make check Python prerequisite unavailable")
	}
	cmd := exec.Command(python, "-B", "../../examples/workers/rcp-toolchain/test_profile.py", "-v")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("operator profile guards: %v\n%s", err, out)
	}
}

// Exercise the prepared image's actual fixed wrapper as well as the fast
// manifest tests. Every case fails before make/check, with no writable image,
// shared cache, credentials, source execution or network.
func TestRealApplicationPreparedProfileRejects(t *testing.T) {
	if os.Getenv("RCP_TEST_DOCKER") != "1" || os.Getenv("RCP_TEST_REALAPP_CONFIG") == "" {
		t.Skip("explicit prepared real-application toolchain and Docker fixture opt-in required")
	}
	var p realApplicationProfile
	b, err := os.ReadFile(os.Getenv("RCP_TEST_REALAPP_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	for _, name := range []string{"Makefile", "go.mod", "go.sum", "package.json", "package-lock.json"} {
		if err = os.WriteFile(filepath.Join(source, name), []byte("corrupt locked source"), 0444); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		extra  []string
		hash   string
		scope  []string
		reason string
	}{
		{"manifest", nil, strings.Repeat("0", 64), nil, "profile manifest mismatch"},
		{"product", nil, "", []string{"other", "source", "trusted_source", "linux/arm64"}, "profile scope mismatch"},
		{"repository", nil, "", []string{"p", "other", "trusted_source", "linux/arm64"}, "profile scope mismatch"},
		{"trust", nil, "", []string{"p", "source", "other", "linux/arm64"}, "profile scope mismatch"},
		{"platform", nil, "", []string{"p", "source", "trusted_source", "linux/amd64"}, "profile scope mismatch"},
		{"missing-tools", []string{"-e", "PATH=/usr/bin:/bin"}, "", nil, "operator profile failed closed"},
		{"missing-dependencies", []string{"--tmpfs", "/opt/rcp/gomod:rw,size=1048576"}, "", nil, "immutable dependency/cache input mismatch: gomod"},
		{"missing-seed", []string{"--tmpfs", "/opt/rcp/cache:rw,size=1048576"}, "", nil, "immutable dependency/cache input mismatch: cache"},
		{"lock", nil, "", nil, "source lock/entrypoint mismatch:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hash := tc.hash
			if hash == "" {
				hash = p.ProfileHash
			}
			scope := tc.scope
			if scope == nil {
				scope = []string{"p", "source", "trusted_source", "linux/arm64"}
			}
			args := []string{"--context", "orbstack", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "512m", "--cpus", "1", "--pids-limit", "64", "--mount", "type=bind,source=" + source + ",target=/source,readonly", "--entrypoint", "/opt/rcp/profile.py"}
			args = append(args, tc.extra...)
			args = append(args, p.Toolchain, "check", "seeded", hash)
			args = append(args, scope...)
			out, err := exec.Command("docker", args...).CombinedOutput()
			if err == nil || !bytes.Contains(out, []byte(tc.reason)) {
				t.Fatalf("prepared profile did not reject %s: %v %s", tc.name, err, out)
			}
		})
	}
}

type realApplicationProvider struct {
	*nativePRFixture
	base string
}

func (p *realApplicationProvider) DiscoverRepositories(context.Context, delivery.Connection) ([]delivery.RepositoryInfo, error) {
	return []delivery.RepositoryInfo{{ID: 11, FullName: "deadnodes/ai-development-workspace", DefaultBranch: "main", URL: "https://github.com/deadnodes/ai-development-workspace", Private: true}, {ID: 22, FullName: "deadnodes/gitops-fixture", DefaultBranch: "main", URL: "https://github.com/deadnodes/gitops-fixture", Private: true}}, nil
}
func (p *realApplicationProvider) Branches(context.Context, delivery.Connection, string) ([]delivery.BranchInfo, error) {
	return []delivery.BranchInfo{{Name: "main", SHA: p.base}, {Name: "feature/work", SHA: p.sha}}, nil
}

func realApplicationCommand(t *testing.T, endpoint, token string, c domain.Command, out any) {
	t.Helper()
	c.Actor = "fixture/operator"
	if c.Data == nil {
		c.Data = map[string]any{}
	}
	body, _ := json.Marshal(c)
	request, _ := http.NewRequest("POST", endpoint+"/api/commands", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, e := http.DefaultClient.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	body, _ = io.ReadAll(response.Body)
	if response.StatusCode != 200 {
		t.Fatalf("%s returned%d:%s", c.Action, response.StatusCode, body)
	}
	if out != nil {
		if e = json.Unmarshal(body, out); e != nil {
			t.Fatal(e)
		}
	}
}

// No replacement clone: copy the exact commit/tree objects from the existing
// checkout into a private fixture mirror, preserving the real source SHA and
// ancestry needed by the supervisor's fresh-view strict object verification.
func realApplicationMirror(t *testing.T, root, source string) (string, func() string) {
	t.Helper()
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	mirror := filepath.Join(root, "source.git")
	run := func(dir string, input []byte, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Stdin = bytes.NewReader(input)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("fixture Git:%v %s", e, out)
		}
		return out
	}
	run(repo, nil, "init", "--bare", mirror)
	objects := run(repo, nil, "rev-list", "--objects", "--no-object-names", source)
	pack := run(repo, objects, "pack-objects", "--stdout")
	run(mirror, pack, "index-pack", "--stdin")
	run(mirror, nil, "update-ref", "refs/heads/feature/work", source)
	changed := func() string {
		index := filepath.Join(root, "fixture-index")
		command := func(input []byte, args ...string) []byte {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = mirror
			cmd.Stdin = bytes.NewReader(input)
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_AUTHOR_DATE=2026-10-22T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-22T00:00:00Z")
			out, e := cmd.CombinedOutput()
			if e != nil {
				t.Fatalf("changed fixture Git:%v %s", e, out)
			}
			return out
		}
		original := run(mirror, nil, "show", source+":web/static/app.js")
		original = append(original, []byte("\n// accepted393 changed-source fixture\n")...)
		blob := strings.TrimSpace(string(command(original, "hash-object", "-w", "--stdin")))
		command(nil, "read-tree", source)
		command(nil, "update-index", "--add", "--cacheinfo", "100644,"+blob+",web/static/app.js")
		tree := strings.TrimSpace(string(command(nil, "write-tree")))
		sha := strings.TrimSpace(string(command([]byte("Changed embedded asset fixture\n"), "commit-tree", tree, "-p", source)))
		run(mirror, nil, "update-ref", "refs/heads/feature/work", sha)
		return sha
	}
	return mirror, changed
}

func TestRealApplicationOfflineNativeDelivery(t *testing.T) {
	configPath := os.Getenv("RCP_TEST_REALAPP_CONFIG")
	if os.Getenv("RCP_TEST_DOCKER") != "1" || configPath == "" {
		t.Skip("explicit prepared real-application toolchain and Docker fixture opt-in required")
	}
	var profile realApplicationProfile
	raw, e := os.ReadFile(configPath)
	if e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(raw, &profile) != nil || !workerconfig.ValidFingerprint("sha256:"+profile.ProfileHash) || profile.Source != "fdfcafb63a1f4433e8d62888553a50d46d951245" {
		t.Fatal("invalid pinned operator profile")
	}
	ctx := context.Background()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	docker := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("docker", append([]string{"--context", "orbstack"}, args...)...)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("owned Docker fixture:%v %s", e, out)
		}
		return strings.TrimSpace(string(out))
	}
	owner := fmt.Sprintf("realapp-native-%d", time.Now().UnixNano())
	registry := docker("run", "--rm", "-d", "--label", "rcp.fixture.owner="+owner, "--read-only", "--tmpfs", "/var/lib/registry:rw,size=1073741824", "--memory", "512m", "--cpus", "1", "--pids-limit", "128", "-p", "127.0.0.1::5000", "registry@sha256:6c5666b861f3505b116bb9aa9b25175e71210414bd010d92035ff64018f9457e")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(registry) {
		t.Fatal("invalid owned registry identity")
	}
	t.Cleanup(func() {
		if docker("inspect", "--format", `{{.Id}} {{index .Config.Labels "rcp.fixture.owner"}}`, registry) != registry+" "+owner {
			t.Error("registry ownership changed")
			return
		}
		docker("rm", "--force", registry)
	})
	origin := "http://" + docker("port", registry, "5000/tcp")
	t.Logf("owned registry owner=%s id=%s origin=%s", owner, registry, origin)
	for range 50 {
		response, e := http.Get(origin + "/v2/")
		if e == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	mirror, changeSource := realApplicationMirror(t, root, profile.Source)
	binaries := map[string]string{}
	for _, name := range []string{"worker", "publisher"} {
		file := filepath.Join(root, name)
		cmd := exec.Command("go", "build", "-o", file, "../../cmd/"+name)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("build fixture executor:%v %s", e, out)
		}
		binaries[name] = file
	}
	store, e := persistence.Open(ctx, isolatedDatabase(t))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	buildToken := "realapp-build-fixture-token-00000000000001"
	publishToken := "realapp-publish-fixture-token-00000000001"
	control := "realapp-control-fixture-token-00000000001"
	t.Setenv("REALAPP_BUILD_TOKEN", buildToken)
	t.Setenv("REALAPP_PUBLISH_TOKEN", publishToken)
	execution := &domain.WorkerExecution{Kind: "local_oci_build", ToolchainImage: profile.Toolchain, BuildKitImage: profile.BuildKit, Platform: "linux/arm64", RuntimeRootFS: &profile.Runtime, CheckArgv: []string{"/opt/rcp/profile.py", "check", "seeded", profile.ProfileHash, "p", "source", "trusted_source", "linux/arm64"}, BuildArgv: []string{"/opt/rcp/profile.py", "build", "seeded", profile.ProfileHash, "p", "source", "trusted_source", "linux/arm64"}, OutputFile: "release-control", ImagePath: "usr/local/bin/release-control", Entrypoint: []string{"/usr/local/bin/release-control"}, Network: "none", TimeoutSeconds: 1800, MemoryMB: 4096, StorageMB: 4096, CPUs: 4, PIDs: 512, SourceBytes: 32 << 20, OutputBytes: 128 << 20, LogBytes: 1 << 20}
	recipe := workerconfig.Recipe{ID: "realapp-seeded", Revision: "v1", ProductID: "p", RepositoryID: "source", RepositoryURL: "https://github.com/deadnodes/ai-development-workspace", Capability: "local_oci_build", Trust: "trusted_source", Parameters: map[string]workerconfig.Parameter{}, Execution: execution}
	recipe.Hash = workerconfig.RecipeHash(recipe)
	cold := recipe
	cold.ID = "realapp-cold"
	coldExecution := *execution
	coldExecution.CheckArgv = append([]string{}, execution.CheckArgv...)
	coldExecution.BuildArgv = append([]string{}, execution.BuildArgv...)
	coldExecution.CheckArgv[2] = "cold"
	coldExecution.BuildArgv[2] = "cold"
	cold.Execution = &coldExecution
	cold.Hash = workerconfig.RecipeHash(cold)
	publication := workerconfig.PublicationRecipe{ID: "realapp-publish", Revision: "v1", ProductID: "p", RepositoryID: "source", ComponentID: "component", Profile: "realapp", BuildRecipeHash: recipe.Hash, Origin: origin, Repository: "fixture/rcp", MaxBytes: 128 << 20, TimeoutSeconds: 120, Pull: workerconfig.RegistryRead{FixtureLoopbackHTTP: true}}
	publication.Hash = workerconfig.PublicationHash(publication)
	coldPublication := publication
	coldPublication.ID = "realapp-cold-publish"
	coldPublication.BuildRecipeHash = cold.Hash
	coldPublication.Hash = workerconfig.PublicationHash(coldPublication)
	authority, e := workerconfig.New(workerconfig.Config{Revision: "realapp1", LeaseSeconds: 300, Recipes: []workerconfig.Recipe{recipe, cold}, Publications: []workerconfig.PublicationRecipe{publication, coldPublication}, Credentials: []workerconfig.Credential{{ID: "build", Principal: "worker/build", TokenEnv: "REALAPP_BUILD_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"local_oci_build"}, ExpiresAt: time.Now().Add(2 * time.Hour)}, {ID: "publisher", Principal: "worker/publisher", TokenEnv: "REALAPP_PUBLISH_TOKEN", ProductIDs: []string{"p"}, RepositoryIDs: []string{"source"}, Capabilities: []string{"registry_publish"}, ExpiresAt: time.Now().Add(2 * time.Hour)}}}, control, os.Getenv)
	if e != nil {
		t.Fatal(e)
	}
	provider := &realApplicationProvider{nativePRFixture: &nativePRFixture{persistedDeliveryFixture: &persistedDeliveryFixture{}, sha: profile.Source}, base: profile.Source}
	service := application.NewWithProvider(store, provider)
	service.ConfigureWorkers(authority)
	server := httptest.NewServer(transport.New(service, transport.Options{Token: control, Workers: authority}))
	defer server.Close()
	post := func(c domain.Command, out any) { t.Helper(); realApplicationCommand(t, server.URL, control, c, out) }
	for _, c := range []domain.Command{{Action: "create_product", ID: "p", Data: map[string]any{"name": "actual RCP source fixture"}}, {Action: "create_feature", ID: "f", ProductID: "p", Data: map[string]any{"title": "actual RCP", "goal": "offline real application acceptance"}}, {Action: "create_integration", ID: "i", FeatureID: "f", Data: map[string]any{"title": "actual RCP", "objective": "exact immutable source"}}, {Action: "create_github_connection", ID: "conn", ProductID: "p", Data: map[string]any{"name": "fake provider", "app_id": 1, "installation_id": 2, "private_key_ref": "env:UNUSED_FIXTURE_KEY", "owner": "deadnodes"}}} {
		post(c, nil)
	}
	if _, e = service.Query(ctx, "discover_repositories", "conn"); e != nil {
		t.Fatal(e)
	}
	for _, r := range []struct{ id, name, role string }{{"source", "deadnodes/ai-development-workspace", "APPLICATION"}, {"gitops", "deadnodes/gitops-fixture", "GITOPS"}} {
		post(domain.Command{Action: "import_repository", ID: r.id, ProductID: "p", Data: map[string]any{"connection_id": "conn", "full_name": r.name, "role": r.role, "default_branch": "main"}}, nil)
	}
	post(domain.Command{Action: "create_application", ID: "component", ProductID: "p", Data: map[string]any{"name": "actual RCP", "repository_id": "source"}}, nil)
	configure := func(r workerconfig.Recipe, p workerconfig.PublicationRecipe) {
		post(domain.Command{Action: "configure_component", ProductID: "p", Data: map[string]any{"application_id": "component", "connection_id": "conn", "mode": "NATIVE", "profile": "realapp", "image_repository": strings.TrimPrefix(origin, "http://") + "/fixture/rcp", "native": map[string]any{"build_recipe_id": r.ID, "publication_recipe_id": p.ID, "parameters": map[string]any{}}}}, nil)
	}
	configure(recipe, publication)
	post(domain.Command{Action: "create_environment", ID: "dev", ProductID: "p", Data: map[string]any{"name": "private fixture"}}, nil)
	post(domain.Command{Action: "configure_environment", ProductID: "p", Data: map[string]any{"environment_id": "dev", "application_id": "component", "purpose": "DEV", "connection_id": "conn", "repository_id": "gitops", "ref": "main", "path": "rcp.yaml", "image_field": "spec.values.image.repository", "digest_field": "spec.values.image.tag", "allow_deploy": true, "gitops_mode": "PR"}}, nil)
	spoolRoot := filepath.Join(root, "spool")
	os.Mkdir(spoolRoot, 0700)
	spool := &artifactspool.Config{Root: spoolRoot, MaxArchiveBytes: 128 << 20, MaxTotalBytes: 512 << 20, MaxEntries: 8}
	home, _ := os.UserHomeDir()
	workerConfig := workerrunner.Config{ArtifactSpool: spool, Endpoint: server.URL, TokenEnv: "REALAPP_BUILD_TOKEN", ProductID: "p", RepositoryID: "source", ConfigRevision: "realapp1", Recipe: recipe, SourceMirror: mirror, StateDir: filepath.Join(root, "worker-seeded"), DockerContext: "orbstack", DockerConfig: filepath.Join(home, ".docker"), AllowRootlessUnconfined: true}
	workerFile := filepath.Join(root, "worker-seeded.json")
	writeConfig := func(path string, v any) {
		b, _ := json.Marshal(v)
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeConfig(workerFile, workerConfig)
	pubDir := filepath.Join(root, "publisher-seeded")
	os.Mkdir(pubDir, 0700)
	publisherConfig := publishrunner.Config{ArtifactSpool: spool, Endpoint: server.URL, TokenEnv: "REALAPP_PUBLISH_TOKEN", ConfigRevision: "realapp1", Recipe: publication, Publisher: ocipublisher.Config{AllowedOrigin: origin, AllowedRepository: publication.Repository, StateDir: pubDir, MaxArchiveBytes: publication.MaxBytes, TimeoutSeconds: publication.TimeoutSeconds, FixtureLoopbackHTTP: true}}
	pubFile := filepath.Join(root, "publisher-seeded.json")
	writeConfig(pubFile, publisherConfig)
	run := func(binary, file string) {
		t.Helper()
		cmd := exec.Command(binary, "run-once", file)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("actual executor failed:%v %s", e, out)
		}
	}
	tick := func() {
		t.Helper()
		if e := store.Update(ctx, func(st *domain.State) error {
			for i := range st.Operations {
				st.Operations[i].NextAttemptAt = st.Operations[i].UpdatedAt
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
		if _, e := service.Tick(ctx); e != nil {
			t.Fatal(e)
		}
	}
	var firstDigest, changedDigest string
	var runtimeArtifact domain.DeliveryArtifact
	for attempt := 0; attempt < 4; attempt++ {
		sha := profile.Source
		if attempt == 2 {
			sha = changeSource()
		}
		if attempt == 3 {
			configure(cold, coldPublication)
			workerConfig.Recipe = cold
			workerConfig.StateDir = filepath.Join(root, "worker-cold")
			workerFile = filepath.Join(root, "worker-cold.json")
			writeConfig(workerFile, workerConfig)
			publisherConfig.Recipe = coldPublication
			publisherConfig.Publisher.StateDir = filepath.Join(root, "publisher-cold")
			os.Mkdir(publisherConfig.Publisher.StateDir, 0700)
			pubFile = filepath.Join(root, "publisher-cold.json")
			writeConfig(pubFile, publisherConfig)
		}
		provider.sha = sha
		provider.merged = false
		revision := fmt.Sprintf("real-rev%d", attempt)
		post(domain.Command{Action: "record_integration_revision", ID: revision, IntegrationID: "i", Data: map[string]any{"repository_id": "source", "branch": "feature/work", "base_commit": profile.Source, "head_commit": sha, "commits": []string{sha}}}, nil)
		var operation domain.ExternalOperation
		post(domain.Command{Action: "deploy_integration", IntegrationID: "i", Data: map[string]any{"environment_id": "dev", "application_id": "component", "revision_id": revision, "request_key": fmt.Sprintf("realapp-native-delivery-%d", attempt)}}, &operation)
		for range 3 {
			tick()
		}
		started := time.Now()
		run(binaries["worker"], workerFile)
		elapsed := time.Since(started)
		tick()
		run(binaries["publisher"], pubFile)
		for range 5 {
			tick()
		}
		st, _ := store.Read(ctx)
		artifact := st.DeliveryArtifacts[len(st.DeliveryArtifacts)-1]
		if artifact.SourceCommit != sha || artifact.Native == nil || artifact.Native.Receipt.RuntimeRootFS == nil || *artifact.Native.Receipt.RuntimeRootFS != profile.Runtime {
			t.Fatal("actual application runtime provenance missing")
		}
		if attempt == 0 {
			firstDigest = artifact.Digest
		}
		if attempt == 1 && artifact.Digest != firstDigest {
			t.Fatal("independent same-source seeded build differs")
		}
		if attempt == 2 {
			changedDigest = artifact.Digest
			runtimeArtifact = artifact
			if artifact.Digest == firstDigest {
				t.Fatal("changed embedded source reused old output")
			}
		}
		if attempt == 3 && artifact.Digest != firstDigest {
			t.Fatal("cold/seeded same-input outputs differ")
		}
		var current domain.ExternalOperation
		for _, op := range st.Operations {
			if op.ID == operation.ID {
				current = op
			}
		}
		if current.Phase != "WAIT_GITOPS_PR" {
			t.Fatalf("real application did not reach exact PR:%+v", current)
		}
		t.Logf("actual source=%s profile=%s check/build/spool_seconds=%.3f manifest=%s archive_bytes=%d", sha, workerConfig.Recipe.ID, elapsed.Seconds(), artifact.Digest, artifact.Native.Receipt.ArchiveBytes)
		provider.merged = true
		tick()
	}
	if provider.dispatches.Load() != 0 || provider.inspections.Load() != 0 || provider.applies.Load() != 0 || provider.proposals.Load() != 4 || changedDigest == "" {
		t.Fatal("real application used fallback/direct/duplicate paths")
	}
	runRealApplicationRuntime(t, docker, root, owner, origin, runtimeArtifact, configPath)
}
