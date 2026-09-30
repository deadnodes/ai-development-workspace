package agentconnect

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	token, err := ResolveCredential(endpoint, profile, tokenEnv)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: bearerTransport{token: token, next: http.DefaultTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
