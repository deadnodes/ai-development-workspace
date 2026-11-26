package agentconnect

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunWhoami prints the authenticated identity exposed by GET /auth/me.
func RunWhoami(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("release-control whoami", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var raw, profile, tokenEnv string
	flags.StringVar(&raw, "url", "", "Control Plane base URL")
	flags.StringVar(&profile, "profile", "default", "saved agent credential profile")
	flags.StringVar(&tokenEnv, "token-env", "", "bearer token environment variable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	endpoint, err := secureEndpoint(raw)
	if err != nil {
		return err
	}
	token, err := ResolveCredential(endpoint, profile, tokenEnv)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/auth/me", nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("whoami request failed: %s", resp.Status)
	}
	var body map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	if _, ok := body["user_id"]; !ok {
		if p, ok := body["principal"].(map[string]any); ok {
			user := p["id"]
			if p["kind"] == "agent" {
				user = p["owner"]
			}
			body["user_id"] = user
		}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

// RunToken is intended for local hooks that need authenticated HTTP access.
// It emits only the credential bytes on stdout and never includes them in errors.
func RunToken(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("release-control token", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var raw, profile string
	var fd int
	flags.StringVar(&raw, "url", "", "Control Plane base URL")
	flags.StringVar(&profile, "profile", "default", "saved agent credential profile")
	flags.IntVar(&fd, "stdout-to-fd", -1, "write credential to this file descriptor instead of stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	endpoint, err := secureEndpoint(raw)
	if err != nil {
		return err
	}
	token, err := loadCredential(endpoint, profile)
	if err != nil {
		return errors.New("saved credential unavailable")
	}
	if fd >= 0 {
		_, err = syscall.Write(fd, []byte(token))
		return err
	}
	_, err = io.WriteString(out, token)
	return err
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(clone)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// RunMCP exposes the remote Control Plane tools and resources on stdio using
// SDK transports at both ends. Stdout is exclusively MCP protocol traffic.
// Restarting the process reloads its persistent agent credential; it requires
// neither a browser session nor an environment inherited by the Codex process.
func RunMCP(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	flags := flag.NewFlagSet("release-control mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var raw, profile, tokenEnv string
	flags.StringVar(&raw, "url", "", "Control Plane base URL")
	flags.StringVar(&profile, "profile", "default", "saved agent credential profile")
	flags.StringVar(&tokenEnv, "token-env", "", "bearer token environment variable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	endpoint, err := secureEndpoint(raw)
	if err != nil {
		return err
	}
	var token string
	if tokenEnv != "" {
		token, err = ResolveCredential(endpoint, "", tokenEnv)
	} else {
		if profile == "" {
			profile = "default"
		}
		token, err = loadCredential(endpoint, profile)
		if errors.Is(err, os.ErrNotExist) && profile == "default" && isLoopbackEndpoint(endpoint) {
			err = nil
		} else if errors.Is(err, os.ErrNotExist) {
			return errors.New("saved credential unavailable; run release-control login")
		}
	}
	if err != nil {
		return err
	}
	transport := http.RoundTripper(http.DefaultTransport)
	if token != "" {
		transport = bearerTransport{token: token, next: http.DefaultTransport}
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	bootstrap, cancelBootstrap := context.WithTimeout(ctx, 30*time.Second)
	defer cancelBootstrap()
	remote, err := mcp.NewClient(&mcp.Implementation{Name: "release-control-agent", Version: "1"}, nil).Connect(bootstrap, &mcp.StreamableClientTransport{Endpoint: endpoint + "/mcp", HTTPClient: client}, nil)
	if err != nil {
		return errors.New("cannot connect to remote MCP; check URL and agent authorization")
	}
	defer remote.Close()
	proxy := mcp.NewServer(&mcp.Implementation{Name: "release-control", Version: "1"}, nil)
	for tool, err := range remote.Tools(bootstrap, nil) {
		if err != nil {
			return errors.New("cannot load remote MCP tools")
		}
		proxy.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return remote.CallTool(ctx, &mcp.CallToolParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
		})
	}
	if remote.InitializeResult().Capabilities.Resources != nil {
		for resource, err := range remote.Resources(bootstrap, nil) {
			if err != nil {
				return errors.New("cannot load remote MCP resources")
			}
			proxy.AddResource(resource, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return remote.ReadResource(ctx, &mcp.ReadResourceParams{URI: req.Params.URI})
			})
		}
	}
	if remote.InitializeResult().Capabilities.Prompts != nil {
		for prompt, err := range remote.Prompts(bootstrap, nil) {
			if err != nil {
				return errors.New("cannot load remote MCP prompts")
			}
			proxy.AddPrompt(prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return remote.GetPrompt(ctx, &mcp.GetPromptParams{Name: req.Params.Name, Arguments: req.Params.Arguments})
			})
		}
	}
	cancelBootstrap()
	reader, ok := in.(io.ReadCloser)
	if !ok {
		reader = io.NopCloser(in)
	}
	return proxy.Run(ctx, &mcp.IOTransport{Reader: reader, Writer: nopWriteCloser{out}})
}

func isLoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
}
