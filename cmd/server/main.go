package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"releasecontrol/internal/agentconnect"
	"releasecontrol/internal/application"
	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/buildregistry"
	"releasecontrol/internal/delivery"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/intelligence"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/prodjwt"
	"releasecontrol/internal/providers/forgejo"
	"releasecontrol/internal/providers/github"
	"releasecontrol/internal/providers/runtimeobserver"
	"releasecontrol/internal/runtimereport"
	"releasecontrol/internal/sqlitefile"
	"releasecontrol/internal/transport"
	"releasecontrol/internal/workerconfig"
)

func main() {
	if err := run(); err != nil {
		name := "server"
		if len(os.Args) > 1 {
			name = os.Args[1]
		}
		slog.Error(name+" failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			return agentconnect.RunLogin(context.Background(), os.Args[2:], os.Stdout)
		case "mcp":
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agentconnect.RunMCP(ctx, os.Args[2:], os.Stdin, os.Stdout, runLocalMCP)
		case "token":
			return agentconnect.RunToken(os.Args[2:], os.Stdout)
		case "doctor":
			return agentconnect.RunDoctor(context.Background(), os.Args[2:], os.Stdout)
		case "whoami":
			return agentconnect.RunWhoami(os.Args[2:], os.Stdout)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		return agentconnect.Run(context.Background(), os.Args[2:], os.Stdout)
	}
	dataPath, addr, workspaceRoot := os.Getenv("RCP_DATA_PATH"), os.Getenv("LISTEN_ADDR"), os.Getenv("RCP_WORKSPACE_ROOT")
	dbURL := os.Getenv("DATABASE_URL")
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		flags := flag.NewFlagSet("release-control serve", flag.ContinueOnError)
		flags.StringVar(&dataPath, "data", dataPath, "local SQLite file")
		flags.StringVar(&addr, "listen", addr, "HTTP listen address")
		flags.StringVar(&workspaceRoot, "workspace", workspaceRoot, "local workspace directory")
		if err := flags.Parse(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		var explicitData bool
		flags.Visit(func(f *flag.Flag) { explicitData = explicitData || f.Name == "data" })
		if explicitData && dbURL != "" {
			return errors.New("--data selects SQLite; unset DATABASE_URL to use it")
		}
	}
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	if dbURL == "" && dataPath == "" {
		var err error
		dataPath, err = sqlitefile.DefaultPath()
		if err != nil {
			return err
		}
	}
	authConfig, err := auth.ConfigFromEnvForData(dataPath)
	if err != nil {
		return err
	}
	localAdmin := dbURL == "" && authConfig.Mode != "oidc" && os.Getenv("RC_TOKEN") == ""
	if localAdmin {
		addr, err = standaloneListenAddress(addr)
		if err != nil {
			return err
		}
	}
	reporterToken, err := runtimeReporterConfig()
	if err != nil {
		return err
	}
	promoterOptions, err := prodPromoterConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Each startup phase has its own budget and logs its duration: a single shared
	// 20s budget made a slow OIDC discovery or cold state load fail the whole boot
	// with an anonymous "context deadline exceeded".
	phase := func(name string, budget time.Duration, fn func(context.Context) error) error {
		started := time.Now()
		phaseCtx, cancelPhase := context.WithTimeout(ctx, budget)
		defer cancelPhase()
		if e := fn(phaseCtx); e != nil {
			slog.Error("startup phase failed", "phase", name, "duration", time.Since(started).Round(time.Millisecond), "budget", budget, "error", e)
			return fmt.Errorf("startup %s: %w", name, e)
		}
		slog.Info("startup phase", "phase", name, "duration", time.Since(started).Round(time.Millisecond))
		return nil
	}
	bootBudget := envDuration("RCP_BOOT_PHASE_TIMEOUT", 20*time.Second)
	var authManager *auth.Manager
	if err = phase("auth", bootBudget, func(c context.Context) (e error) { authManager, e = auth.New(c, authConfig); return e }); err != nil {
		return err
	}
	defer authManager.Close()
	var store interface {
		application.Store
		Close()
	}
	if dbURL != "" {
		err = phase("database", bootBudget, func(c context.Context) (e error) {
			store, e = persistence.Open(c, dbURL)
			return e
		})
	} else {
		err = phase("database", bootBudget, func(c context.Context) (e error) {
			store, e = persistence.OpenSQLite(c, dataPath)
			return e
		})
		if err == nil {
			slog.Info("using local database", "path", dataPath)
		}
	}
	if err != nil {
		return err
	}
	defer store.Close()
	githubProvider := github.New()
	service := application.NewWithProvider(store, githubProvider)
	if keyPath := os.Getenv("RCP_PROD_PUBLIC_KEY_FILE"); keyPath != "" {
		pem, e := os.ReadFile(keyPath)
		if e != nil {
			return fmt.Errorf("read production public key: %w", e)
		}
		if e = service.ConfigureProdPublicKey(string(pem)); e != nil {
			return fmt.Errorf("configure production public key: %w", e)
		}
	}
	if err := phase("connection secrets", envDuration("RCP_STATE_LOAD_TIMEOUT", 2*time.Minute), service.CheckConnectionSecrets); err != nil {
		return err
	}
	if path := os.Getenv("RCP_BUILDS_FILE"); path != "" {
		config, e := buildconfig.Load(path)
		if e != nil {
			return fmt.Errorf("load build authority: %w", e)
		}
		forgejoProvider, registryReader := forgejo.New(), buildregistry.New()
		if e = service.ConfigureBuilds(config, forgejoProvider, registryReader); e != nil {
			return e
		}
		var runtime application.DeploymentRuntime
		if config.Rollout.InCluster {
			runtime, e = runtimeobserver.NewRolloutVerifier()
			if e != nil {
				return fmt.Errorf("configure deployment rollout verifier: %w", e)
			}
		} else {
			configured := false
			for _, profile := range config.Profiles {
				for _, file := range profile.Deployments["dev"].Files {
					configured = configured || file.Rollout != nil
				}
			}
			if configured {
				slog.Warn("rollout targets configured without a verifier; deployments will succeed without runtime verification")
			}
		}
		if e = service.ConfigureDeployments(forgejoProvider, registryReader, githubProvider, runtime); e != nil {
			return e
		}
		done := make(chan struct{})
		go func() { defer close(done); service.RunBuilds(ctx) }()
		defer func() { stop(); <-done }()
	}

	service.SetWorkspaceRoot(workspaceRoot)
	if path := os.Getenv("RCP_INTELLIGENCE_FILE"); path != "" {
		authority, e := intelligence.Load(path)
		if e != nil {
			return fmt.Errorf("load intelligence authority: %w", e)
		}
		service.ConfigureIntelligence(authority)
	}
	var workers *workerconfig.Authority
	if path := os.Getenv("RCP_WORKERS_FILE"); path != "" {
		workers, err = workerconfig.Load(path, os.Getenv("RC_TOKEN"))
		if err != nil {
			return fmt.Errorf("load worker authority: %w", err)
		}
		service.ConfigureWorkers(workers)
	}
	if dbURL == "" && workspaceRoot != "" {
		if err := phase("workspace scan", bootBudget, func(c context.Context) error { return bootstrapWorkspace(c, service, workspaceRoot) }); err != nil {
			slog.Warn("initial workspace scan incomplete; server will remain available", "error", err)
		}
	}
	runtimeRetention := 168 * time.Hour
	if raw := os.Getenv("RCP_RUNTIME_OBSERVATION_RETENTION"); raw != "" {
		runtimeRetention, err = time.ParseDuration(raw)
		if err != nil || runtimeRetention < 0 || runtimeRetention > 0 && runtimeRetention < 24*time.Hour {
			return fmt.Errorf("RCP_RUNTIME_OBSERVATION_RETENTION must be 0 or a duration of at least 24h")
		}
	}
	operationRetention := 720 * time.Hour
	if raw := os.Getenv("RCP_OPERATION_RETENTION"); raw != "" {
		operationRetention, err = time.ParseDuration(raw)
		if err != nil || operationRetention < 0 || operationRetention > 0 && operationRetention < 72*time.Hour {
			return fmt.Errorf("RCP_OPERATION_RETENTION must be 0 or a duration of at least 72h")
		}
	}
	refreshOperationRetention := 168 * time.Hour
	if raw := os.Getenv("RCP_REFRESH_OPERATION_RETENTION"); raw != "" {
		refreshOperationRetention, err = time.ParseDuration(raw)
		if err != nil || refreshOperationRetention < 0 || refreshOperationRetention > 0 && refreshOperationRetention < 24*time.Hour {
			return fmt.Errorf("RCP_REFRESH_OPERATION_RETENTION must be 0 or a duration of at least 24h")
		}
	}
	// Retention also runs when observers are paused or not configured.
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		timer := time.NewTimer(time.Hour)
		defer timer.Stop()
		for {
			if err := service.PruneRuntimeObservations(ctx, time.Now().UTC(), runtimeRetention); err != nil && ctx.Err() == nil {
				slog.Warn("runtime observation retention failed", "error", err)
			}
			deleted, pruneErr := service.PruneOperations(ctx, time.Now().UTC(), operationRetention, refreshOperationRetention)
			if pruneErr != nil && ctx.Err() == nil {
				slog.Warn("operation retention failed", "error", pruneErr)
			}
			timer.Reset(operationRetentionDelay(deleted))
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()
	defer func() { stop(); <-retentionDone }()
	if path := os.Getenv("RCP_OBSERVERS_FILE"); path != "" {
		configuredObservers, err := runtimeobserver.Load(path)
		if err != nil {
			return fmt.Errorf("load runtime observers: %w", err)
		}
		observers := make([]delivery.RuntimeObserver, len(configuredObservers))
		for i, o := range configuredObservers {
			observers[i] = o
		}
		snapshots := make([]delivery.RuntimeSnapshotObserver, len(configuredObservers))
		for i, o := range configuredObservers {
			snapshots[i] = o
		}
		service.SetRuntimeObservers(snapshots)
		observerDone := make(chan struct{})
		go func() {
			defer close(observerDone)
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				if err := service.ObserveRuntimeOnce(ctx, observers); err != nil && ctx.Err() == nil {
					slog.Warn("runtime observation failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		defer func() { stop(); <-observerDone }()
	}
	gitInterval := 2 * time.Minute
	if raw := os.Getenv("RCP_GIT_SYNC_INTERVAL"); raw != "" {
		var err error
		gitInterval, err = time.ParseDuration(raw)
		if err != nil || gitInterval < 0 || (gitInterval > 0 && gitInterval < 30*time.Second) {
			return fmt.Errorf("RCP_GIT_SYNC_INTERVAL must be 0 or a duration of at least 30s")
		}
	}
	gitDone := make(chan struct{})
	executionDone := make(chan struct{})
	executionInterval := service.ExecutionReconcileInterval()
	go func() {
		defer close(executionDone)
		if executionInterval == 0 {
			return
		}
		ticker := time.NewTicker(executionInterval)
		defer ticker.Stop()
		for {
			if err := service.ReconcileExecutionsOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("execution reconciliation failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { stop(); <-executionDone }()
	go func() {
		defer close(gitDone)
		if err := service.RunGitScheduler(ctx, gitInterval); err != nil && ctx.Err() == nil {
			slog.Error("Git synchronization scheduler stopped", "error", err)
		}
	}()
	defer func() { stop(); <-gitDone }()
	workerEnabled := os.Getenv("RCP_WORKER_ENABLED")
	if workerEnabled != "" && workerEnabled != "true" && workerEnabled != "false" {
		return fmt.Errorf("RCP_WORKER_ENABLED must be true or false")
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if workerEnabled == "false" {
			return
		}
		if err := service.RunWorker(ctx, 2*time.Second); err != nil && ctx.Err() == nil {
			slog.Error("operation worker stopped", "error", err)
		}
	}()
	defer func() { stop(); <-workerDone }()
	server := &http.Server{Addr: addr, Handler: transport.New(service, transport.Options{LocalAdmin: localAdmin, RuntimeReporterToken: reporterToken, RuntimeReporterScope: os.Getenv("RCP_RUNTIME_REPORTER_SCOPE"), ProdPromoterKeys: promoterOptions.keys, ProdPromoterIssuer: promoterOptions.issuer, ProdPromoterAudience: promoterOptions.audience, ProdPromoterSubject: promoterOptions.subject, ProdPromoterScope: promoterOptions.scope, Auth: authManager, Workers: workers, Token: os.Getenv("RC_TOKEN"), AllowedHosts: strings.FieldsFunc(os.Getenv("ALLOWED_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' })}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { slog.Info("release-control listening", "address", addr); done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

// runtimeReporterConfig binds the single reporter credential to at most one scope.
func runtimeReporterConfig() (string, error) {
	rawScope := os.Getenv("RCP_RUNTIME_REPORTER_SCOPE")
	if _, err := runtimereport.ParseScopes(rawScope); err != nil {
		return "", fmt.Errorf("RCP_RUNTIME_REPORTER_SCOPE: %w", err)
	}
	if strings.Contains(rawScope, ",") {
		return "", fmt.Errorf("RCP_RUNTIME_REPORTER_SCOPE must contain exactly one product:environment:cluster entry for the single RCP_RUNTIME_REPORTER_TOKEN_FILE; additional clusters require future per-scope tokens")
	}
	reporterToken := ""
	if path := os.Getenv("RCP_RUNTIME_REPORTER_TOKEN_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			reporterToken = strings.TrimSpace(string(b))
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("runtime reporter token file unreadable: %w", err)
		}
	}
	return reporterToken, nil
}

type prodPromoterSettings struct {
	keys                             prodjwt.Keys
	issuer, audience, subject, scope string
}

func prodPromoterConfig() (prodPromoterSettings, error) {
	var c prodPromoterSettings
	path := strings.TrimSpace(os.Getenv("RCP_PROD_PROMOTER_JWKS_FILE"))
	if path == "" {
		return c, nil
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return c, fmt.Errorf("read production promoter JWKS: %w", e)
	}
	if len(b) > 1<<20 {
		return c, fmt.Errorf("production promoter JWKS exceeds limit")
	}
	c.keys, e = prodjwt.LoadJWKS(b)
	if e != nil {
		return c, fmt.Errorf("invalid production promoter JWKS")
	}
	c.issuer = strings.TrimSpace(os.Getenv("RCP_PROD_PROMOTER_ISSUER"))
	if c.issuer == "" {
		return c, fmt.Errorf("RCP_PROD_PROMOTER_ISSUER required when JWKS is configured")
	}
	c.audience = os.Getenv("RCP_PROD_PROMOTER_AUDIENCE")
	if c.audience == "" {
		c.audience = "rcp-prod-promoter"
	}
	c.subject = os.Getenv("RCP_PROD_PROMOTER_SUBJECT")
	if c.subject == "" {
		c.subject = "system:serviceaccount:supply-chain:cosign-prod-signer"
	}
	c.scope = os.Getenv("RCP_PROD_PROMOTER_SCOPE")
	product, environment, ok := strings.Cut(c.scope, ":")
	if !ok || product == "" || environment == "" || strings.Contains(environment, ":") {
		return c, fmt.Errorf("RCP_PROD_PROMOTER_SCOPE must be product_id:environment_id")
	}
	return c, nil
}

// Bootstrap is local-only and runs once. A failed scan retains the Product so a
// restart cannot silently rescan or overwrite work recorded after startup.
func bootstrapWorkspace(ctx context.Context, service *application.Service, root string) error {
	state, err := service.StatePage(ctx, domain.StatePageRequest{Limit: 1})
	if err != nil {
		return err
	}
	if len(state.Items) != 0 {
		return nil
	}
	canonical, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace root must be a directory")
	}
	product, err := service.InitializeWorkspaceProduct(ctx, filepath.Base(canonical))
	if err != nil || product.ID == "" {
		return err
	}
	_, err = service.ScanWorkspace(ctx, product.ID, "system/workspace")
	return err
}

// operationRetentionDelay bounds catch-up to one batch per minute.
func operationRetentionDelay(deleted int) time.Duration {
	if deleted >= application.OperationPruneLimit {
		return time.Minute
	}
	return time.Hour
}

// envDuration reads a positive Go duration from name, falling back to def when
// the variable is unset or invalid (logged).
func envDuration(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration; using default", "variable", name, "default", def)
		return def
	}
	return d
}

func standaloneListenAddress(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil && strings.EqualFold(host, "localhost") {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return "", errors.New("SQLite without authentication requires a loopback LISTEN_ADDR; configure RC_TOKEN or OIDC for non-loopback access")
	}
	return addr, nil
}

type closeWriter struct{ io.Writer }

func (closeWriter) Close() error { return nil }

func runLocalMCP(ctx context.Context, data, workspace string, in io.Reader, out io.Writer) error {
	store, err := persistence.OpenSQLite(ctx, data)
	if err != nil {
		return err
	}
	defer store.Close()
	service := application.New(store)
	if workspace == "" {
		workspace = os.Getenv("RCP_WORKSPACE_ROOT")
	}
	service.SetWorkspaceRoot(workspace)
	if workspace != "" {
		if err := bootstrapWorkspace(auth.WithLocalAdmin(ctx), service, workspace); err != nil {
			slog.Warn("initial workspace scan incomplete", "error", err)
		}
	}
	reader, ok := in.(io.ReadCloser)
	if !ok {
		reader = io.NopCloser(in)
	}
	return transport.NewLocalMCP(service).Run(ctx, &mcp.IOTransport{Reader: reader, Writer: closeWriter{out}})
}
