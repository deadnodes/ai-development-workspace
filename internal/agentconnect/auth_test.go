package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCredentialEncryptionPersistenceAndPermissions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const endpoint = "https://control.example.test"
	const token = "agent-secret-never-plaintext"
	if err := saveCredential(endpoint, "default", token); err != nil {
		t.Fatal(err)
	}
	// Reopen all state as a subsequent process would.
	got, err := ResolveCredential(endpoint, "default", "")
	if err != nil || got != token {
		t.Fatalf("resolve persisted credential: %q %v", got, err)
	}
	name, _ := credentialName(endpoint, "default")
	data, err := os.ReadFile(filepath.Join(home, ".release-control", name))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("plaintext token persisted")
	}
	for _, name := range []string{name, "key"} {
		info, err := os.Stat(filepath.Join(home, ".release-control", name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("insecure mode: %v %v", info, err)
		}
	}
	if _, err = ResolveCredential(endpoint, "another", ""); err == nil {
		t.Fatal("wrong profile accepted")
	}
	data[len(data)-1] ^= 1
	os.WriteFile(filepath.Join(home, ".release-control", name), data, 0600)
	if _, err = ResolveCredential(endpoint, "default", ""); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestCredentialSymlinkAndLoosePermissionsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".release-control")); err != nil {
		t.Fatal(err)
	}
	if err := saveCredential("https://control.test", "default", "secret"); err == nil {
		t.Fatal("symlink accepted")
	}
	os.Remove(filepath.Join(home, ".release-control"))
	os.Mkdir(filepath.Join(home, ".release-control"), 0755)
	if err := saveCredential("https://control.test", "default", "secret"); err == nil {
		t.Fatal("public directory accepted")
	}
}

func TestSecureEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://remote.example.test", "https://user:secret@host", "https://host/?token=secret", "file:///tmp/a"} {
		if _, err := secureEndpoint(endpoint); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	for _, endpoint := range []string{"https://host", "http://127.0.0.1:8090", "http://[::1]:8090"} {
		if _, err := secureEndpoint(endpoint); err != nil {
			t.Fatalf("rejected %s: %v", endpoint, err)
		}
	}
}

func TestDevicePollingPendingSlowDownAndFailures(t *testing.T) {
	for _, terminal := range []string{"success", "access_denied", "expired_token", "unknown"} {
		t.Run(terminal, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				json.NewDecoder(r.Body).Decode(&body)
				if body["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || body["device_code"] != "code" {
					t.Error("invalid poll payload")
				}
				calls++
				w.Header().Set("Content-Type", "application/json")
				code := "authorization_pending"
				if calls == 2 {
					code = "slow_down"
				}
				if calls == 3 {
					code = terminal
				}
				if code == "success" {
					json.NewEncoder(w).Encode(tokenResponse{AccessToken: "secret", TokenType: "Bearer"})
					return
				}
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(tokenResponse{Error: code})
			}))
			defer server.Close()
			var intervals []time.Duration
			token, err := pollDevice(context.Background(), authClient(), server.URL, deviceAuthorization{DeviceCode: "code", ExpiresIn: 60, Interval: 1}, func(_ context.Context, d time.Duration) error { intervals = append(intervals, d); return nil })
			if terminal == "success" {
				if err != nil || token != "secret" {
					t.Fatalf("%s %v", token, err)
				}
			} else if err == nil || token != "" {
				t.Fatal("invalid terminal status accepted")
			}
			if calls != 3 || intervals[0] != time.Second || intervals[2] != 6*time.Second {
				t.Fatalf("poll timing: %v %d", intervals, calls)
			}
		})
	}
}

func TestDevicePollingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pollDevice(ctx, authClient(), "http://127.0.0.1", deviceAuthorization{ExpiresIn: 10, Interval: 1}, waitDevice)
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

func TestConnectSavedCredentialSecretFreeAndIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := kitServer(t, 1, "saved-agent-secret")
	if err := saveCredential(server.URL, "default", "saved-agent-secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	exe, _ := os.Executable()
	o := Options{URL: server.URL, Workspace: root, Executable: exe}
	binding, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Profile != "default" || binding.URL != server.URL {
		t.Fatalf("bad binding: %+v", binding)
	}
	before := readFile(t, root, ".codex/config.toml")
	if !bytes.Contains(before, []byte("command = ")) || !bytes.Contains(before, []byte(server.URL)) || !bytes.Contains(before, []byte("--profile")) {
		t.Fatalf("bad MCP config %s", before)
	}
	if _, err = Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readFile(t, root, ".codex/config.toml")) {
		t.Fatal("reconnect is not idempotent")
	}
	for _, path := range []string{".codex/config.toml", ".release-control.json", "AGENTS.md", ".agents/skills/rcp-handoff/SKILL.md"} {
		if bytes.Contains(readFile(t, root, path), []byte("saved-agent-secret")) {
			t.Fatalf("secret in %s", path)
		}
	}
}

func TestMCPProxyRoundTripAndRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Echo"}, func(ctx context.Context, r *mcp.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
	server.AddResource(&mcp.Resource{URI: "rcp://skill", Name: "skill"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "rcp://skill", Text: "resume first"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer persistent-agent" {
			http.Error(w, "unauthorized", 401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer remote.Close()
	if err := saveCredential(remote.URL, "default", "persistent-agent"); err != nil {
		t.Fatal(err)
	}
	// Two independent proxy starts prove saved credentials work without an env token.
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		proxyIn, clientOut := io.Pipe()
		clientIn, proxyOut := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- RunMCP(ctx, []string{"--url", remote.URL}, proxyIn, proxyOut); proxyOut.Close() }()
		session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		tools, err := session.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "echo" {
			cancel()
			t.Fatalf("list: %+v %v", tools, err)
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hello"}})
		if err != nil || result.Content[0].(*mcp.TextContent).Text != "hello" {
			cancel()
			t.Fatalf("call: %+v %v", result, err)
		}
		resource, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "rcp://skill"})
		if err != nil || resource.Contents[0].Text != "resume first" {
			cancel()
			t.Fatalf("resource: %+v %v", resource, err)
		}
		session.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("proxy did not exit")
		}
	}
}

func TestLoginDoesNotFollowRedirects(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	_, err := postAuth(context.Background(), authClient(), source.URL, map[string]string{"device_code": "secret"}, &tokenResponse{})
	if err == nil || leaked {
		t.Fatal("redirect leaked credential")
	}
}

func TestCredentialErrorsDoNotExposeToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	err := RunMCP(context.Background(), []string{"--url", "https://control.test"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("missing login guidance: %v", err)
	}
}

func TestLoginEnrollsAndSavesWithoutPrintingCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var server *httptest.Server
	var polled bool
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth/device/code":
			var request struct {
				Name, Scope string
				Products    []string
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Name != "workstation" || request.Scope != "read write" || len(request.Products) != 1 || request.Products[0] != "p" {
				t.Errorf("wrong enrollment: %+v", request)
			}
			json.NewEncoder(w).Encode(deviceAuthorization{DeviceCode: "private-device-code", UserCode: "ABCD-EFGH", VerificationURI: server.URL + "/auth/device", VerificationURIComplete: server.URL + "/auth/device?user_code=ABCD-EFGH", ExpiresIn: 60, Interval: 1})
		case "/auth/device/token":
			polled = true
			json.NewEncoder(w).Encode(tokenResponse{AccessToken: "new-agent-secret", TokenType: "Bearer"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	if err := RunLogin(context.Background(), []string{"--url", server.URL, "--name", "workstation", "--products", "p"}, &output); err != nil {
		t.Fatal(err)
	}
	if !polled || !strings.Contains(output.String(), "ABCD-EFGH") || strings.Contains(output.String(), "private-device-code") || strings.Contains(output.String(), "new-agent-secret") {
		t.Fatalf("unsafe/incomplete login output: %q", output.String())
	}
	token, err := ResolveCredential(server.URL, "default", "")
	if err != nil || token != "new-agent-secret" {
		t.Fatalf("persisted credential unavailable: %v", err)
	}
}
