package agentconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/codeindex"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/workspace"
)

type indexBackend interface {
	GetCodeIndexStatus(context.Context, domain.CodeStatusRequest) (domain.CodeStatusResult, error)
	UpsertCodeIndex(context.Context, domain.CodeIndexWrite) (domain.CodeIndexWriteResult, error)
}

type remoteIndex struct {
	endpoint, token string
}

func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (r remoteIndex) call(ctx context.Context, method, route string, input, output any, budget int64) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.endpoint+route, body)
	if err != nil {
		return err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := noRedirectClient(2 * time.Minute)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %s: HTTP %d", method, req.URL.Path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, budget+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > budget {
		return errors.New("RCP response exceeds byte limit")
	}
	return json.Unmarshal(data, output)
}

func (r remoteIndex) GetCodeIndexStatus(ctx context.Context, in domain.CodeStatusRequest) (out domain.CodeStatusResult, err error) {
	query := url.Values{"product_id": {in.ProductID}, "repository_id": {in.RepositoryID}, "branch": {in.Branch}}
	err = r.call(ctx, "GET", "/api/code/status?"+query.Encode(), nil, &out, 128<<10)
	return
}

func (r remoteIndex) UpsertCodeIndex(ctx context.Context, in domain.CodeIndexWrite) (out domain.CodeIndexWriteResult, err error) {
	err = r.call(ctx, "POST", "/api/code/index", in, &out, 128<<10)
	return
}

func (r remoteIndex) ListCodeRepositories(ctx context.Context, product, cursor string) (out domain.CodeRepositoryPage, err error) {
	query := url.Values{"product_id": {product}, "cursor": {cursor}}
	err = r.call(ctx, "GET", "/api/code/repositories?"+query.Encode(), nil, &out, domain.CodeRepositoryPageBytes)
	return
}

type codeRepositoryPager interface {
	ListCodeRepositories(context.Context, string, string) (domain.CodeRepositoryPage, error)
}

func indexRepositories(ctx context.Context, pager codeRepositoryPager, product string) ([]domain.CodeRepository, error) {
	var repos []domain.CodeRepository
	cursor, last := "", ""
	for {
		page, err := pager.ListCodeRepositories(ctx, product, cursor)
		if err != nil {
			return nil, err
		}
		if page.ProductID != product || len(page.Items) > domain.CodeRepositoryPageSize || page.NextCursor != "" && (page.NextCursor == cursor || len(page.Items) == 0) {
			return nil, errors.New("invalid RCP repository page")
		}
		for _, repo := range page.Items {
			if !validProductID(repo.ID) || repo.ID <= last || len(repo.URL) > domain.CodeRepositoryURLBytes {
				return nil, errors.New("invalid RCP repository identity, order or URL")
			}
			last = repo.ID
		}
		repos = append(repos, page.Items...)
		cursor = page.NextCursor
		if cursor == "" {
			return repos, nil
		}
	}
}

func readIndexBinding(dir string) Binding {
	for {
		candidate := filepath.Join(dir, ".release-control.json")
		var binding Binding
		if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() && info.Size() <= 64<<10 {
			if raw, err := os.ReadFile(candidate); err == nil && json.Unmarshal(raw, &binding) == nil {
				return binding
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Binding{}
		}
		dir = parent
	}
}

type indexCheckout struct{ repository, path string }

func matchingIndexCheckouts(ctx context.Context, dir string, repos []domain.CodeRepository, out io.Writer) ([]indexCheckout, error) {
	scan, err := workspace.Scan(ctx, dir)
	if err != nil {
		return nil, err
	}
	if scan.Truncated {
		fmt.Fprintln(out, "Warning: workspace discovery was truncated; use smaller workspace roots")
	}
	byRemote := map[string][]string{}
	for _, repo := range repos {
		if remote := workspace.RemoteIdentity(repo.URL); remote != "" {
			byRemote[remote] = append(byRemote[remote], repo.ID)
		}
	}
	var targets []indexCheckout
	seen := map[[2]string]string{}
	for _, checkout := range scan.Repositories {
		ids := byRemote[workspace.RemoteIdentity(checkout.RemoteURL)]
		if len(ids) > 1 {
			return nil, fmt.Errorf("checkout %q matches multiple attached repositories; use --repo-id and --path", checkout.RelativePath)
		}
		if len(ids) == 1 {
			branch := checkout.Branch
			if branch == "" {
				branch = "@{detached}"
			}
			key := [2]string{ids[0], branch}
			if prior, exists := seen[key]; exists {
				return nil, fmt.Errorf("checkouts %q and %q would overwrite repository %s branch %q; use --repo-id and --path", prior, checkout.RelativePath, ids[0], branch)
			}
			seen[key] = checkout.RelativePath
			targets = append(targets, indexCheckout{ids[0], filepath.Join(scan.Root, filepath.FromSlash(checkout.RelativePath))})
		}
	}
	return targets, nil
}

func RunIndex(ctx context.Context, args []string, out io.Writer) error {
	var endpoint, profile, tokenEnv, data, product, repo, checkout, workspaceRoot, export string
	var dryRun, includeUntracked bool
	flags := flag.NewFlagSet("release-control index", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&repo, "repo-id", "", "attached repository ID for one checkout")
	flags.StringVar(&checkout, "path", "", "local checkout path")
	flags.StringVar(&workspaceRoot, "workspace", "", "scan for attached checkouts under this root")
	flags.StringVar(&product, "product-id", "", "Product ID")
	flags.StringVar(&endpoint, "url", "", "Control Plane base URL")
	flags.StringVar(&profile, "profile", "", "saved credential profile")
	flags.StringVar(&tokenEnv, "token-env", "", "bearer token environment variable")
	flags.StringVar(&data, "data", "", "standalone SQLite database path")
	flags.StringVar(&export, "out", "", "write the filtered index JSON to a new file")
	flags.BoolVar(&dryRun, "dry-run", false, "build the index without uploading it")
	flags.BoolVar(&includeUntracked, "include-untracked", false, "include non-ignored untracked files in Git checkouts")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || workspaceRoot == "" && (repo == "" || checkout == "") || workspaceRoot != "" && (repo != "" || checkout != "" || product == "") {
		return errors.New("use --repo-id ID --path CHECKOUT, or --workspace ROOT --product-id ID")
	}
	if data != "" && (endpoint != "" || profile != "" || tokenEnv != "") {
		return errors.New("--data cannot be combined with remote connection options")
	}
	if export != "" && workspaceRoot != "" {
		return errors.New("--out is supported for a single checkout")
	}
	if export != "" {
		if _, err := os.Lstat(export); !errors.Is(err, os.ErrNotExist) {
			return errors.New("--out requires a new file")
		}
	}
	targets := []indexCheckout{{repo, checkout}}
	var backend indexBackend
	if !dryRun || workspaceRoot != "" {
		location := checkout
		if workspaceRoot != "" {
			location = workspaceRoot
		}
		absolute, err := filepath.Abs(location)
		if err != nil {
			return err
		}
		binding := readIndexBinding(absolute)
		var pager codeRepositoryPager
		if data != "" {
			open := persistence.OpenSQLite
			if dryRun {
				open = persistence.OpenSQLiteReadOnly
			}
			store, err := open(ctx, data)
			if err != nil {
				return err
			}
			defer store.Close()
			service := application.New(store)
			backend, pager = service, service
			if product == "" {
				err = store.View(ctx, func(st *domain.State) error {
					for _, record := range st.Repositories {
						if record.ID == repo {
							product = record.ProductID
							break
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
		} else {
			explicitEndpoint := endpoint != ""
			if endpoint == "" {
				if tokenEnv != "" {
					return errors.New("--token-env requires an explicit --url")
				}
				endpoint = binding.URL
			}
			endpoint, err = secureEndpoint(endpoint)
			if err != nil {
				return err
			}
			if strings.TrimRight(binding.URL, "/") == endpoint {
				if product == "" {
					product = binding.ProductID
				}
				if profile == "" && tokenEnv == "" {
					profile = binding.Profile
				}
			}
			if explicitEndpoint && profile == "" && tokenEnv == "" && os.Getenv("RC_TOKEN") != "" {
				tokenEnv = "RC_TOKEN"
			}
			token, err := ResolveCredential(endpoint, profile, tokenEnv)
			if err != nil {
				return err
			}
			remote := remoteIndex{endpoint, token}
			backend, pager = remote, remote
			if product == "" {
				product, err = discoverProduct(ctx, endpoint, token)
				if err != nil {
					return err
				}
			}
		}
		if product == "" {
			return errors.New("no Product found for repository; specify --product-id")
		}
		if workspaceRoot != "" {
			repos, err := indexRepositories(ctx, pager, product)
			if err != nil {
				return err
			}
			targets, err = matchingIndexCheckouts(ctx, workspaceRoot, repos, out)
			if err != nil {
				return err
			}
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(out, "No checkouts matched attached repositories")
	}
	for _, target := range targets {
		if err := indexTarget(ctx, target, backend, product, export, dryRun, codeindex.Options{IncludeUntracked: includeUntracked}, out); err != nil {
			return fmt.Errorf("index %s: %w", target.repository, err)
		}
	}
	return nil
}

func indexTarget(ctx context.Context, target indexCheckout, backend indexBackend, product, export string, dryRun bool, options codeindex.Options, out io.Writer) error {
	if export != "" {
		root, err := filepath.Abs(target.path)
		if err != nil {
			return err
		}
		destination, err := filepath.Abs(export)
		if err != nil {
			return err
		}
		if rel, err := filepath.Rel(root, destination); err == nil {
			options.Exclude = []string{filepath.ToSlash(rel)}
		}
	}
	doc, err := codeindex.Build(ctx, target.repository, target.path, time.Now(), options)
	if err != nil {
		return err
	}
	digest, err := doc.Digest()
	if err != nil {
		return err
	}
	if export != "" {
		file, err := os.OpenFile(export, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if err := errors.Join(json.NewEncoder(file).Encode(doc), file.Close()); err != nil {
			_ = os.Remove(export)
			return err
		}
	}
	status := "dry-run"
	if !dryRun {
		prior, err := backend.GetCodeIndexStatus(ctx, domain.CodeStatusRequest{ProductID: product, RepositoryID: doc.RepositoryID, Branch: doc.Branch})
		if err != nil {
			return err
		}
		status = "unchanged"
		if len(prior.Items) == 0 || prior.Items[0].RepositoryID != doc.RepositoryID || prior.Items[0].Branch != doc.Branch || prior.Items[0].Digest != digest {
			result, err := backend.UpsertCodeIndex(ctx, domain.CodeIndexWrite{ProductID: product, RepositoryID: doc.RepositoryID, Document: doc})
			if err != nil {
				return err
			}
			if result.Changed {
				status = "uploaded"
			}
		}
	}
	fmt.Fprintf(out, "%s %s %s %s: %d files, digest %s", status, doc.RepositoryID, doc.Branch, doc.Commit, len(doc.Files), digest)
	if len(doc.Truncated) != 0 {
		fmt.Fprintf(out, " (truncated: %s)", strings.Join(doc.Truncated, ", "))
	}
	fmt.Fprintln(out)
	return nil
}
