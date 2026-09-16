package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"releasecontrol/internal/agentconnect"
	"releasecontrol/internal/application"
	"releasecontrol/internal/domain"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/providers/github"
	"releasecontrol/internal/transport"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "connect" {
		return agentconnect.Run(context.Background(), os.Args[2:], os.Stdout)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	boot, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var store interface {
		application.Store
		Close()
	}
	var err error
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		store, err = persistence.Open(boot, dbURL)
	} else {
		dataPath := os.Getenv("RCP_DATA_PATH")
		if dataPath == "" {
			directory, pathErr := os.UserConfigDir()
			if pathErr != nil {
				return pathErr
			}
			dataPath = filepath.Join(directory, "release-control", "state.db")
		}
		store, err = persistence.OpenLocal(boot, dataPath)
		if err == nil {
			slog.Info("using local database", "path", dataPath)
		}
	}
	if err != nil {
		return err
	}
	defer store.Close()
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	service := application.NewWithProvider(store, github.New())
	workspaceRoot := os.Getenv("RCP_WORKSPACE_ROOT")
	service.SetWorkspaceRoot(workspaceRoot)
	if os.Getenv("DATABASE_URL") == "" && workspaceRoot != "" {
		if err := bootstrapWorkspace(boot, service, workspaceRoot); err != nil {
			slog.Warn("initial workspace scan incomplete; server will remain available", "error", err)
		}
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if err := service.RunWorker(ctx, 2*time.Second); err != nil && ctx.Err() == nil {
			slog.Error("operation worker stopped", "error", err)
		}
	}()
	defer func() { stop(); <-workerDone }()
	server := &http.Server{Addr: addr, Handler: transport.New(service, transport.Options{Token: os.Getenv("RC_TOKEN"), AllowedHosts: strings.FieldsFunc(os.Getenv("ALLOWED_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' })}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
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

// Bootstrap is local-only and runs once. A failed scan retains the Product so a
// restart cannot silently rescan or overwrite work recorded after startup.
func bootstrapWorkspace(ctx context.Context, service *application.Service, root string) error {
	state, err := service.State(ctx)
	if err != nil {
		return err
	}
	if len(state.Products) != 0 {
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
	value, err := service.Execute(ctx, domain.Command{Action: "create_product", Actor: "system/workspace", Data: map[string]any{"name": filepath.Base(canonical)}})
	if err != nil {
		return err
	}
	product, ok := value.(domain.Product)
	if !ok || product.ID == "" {
		return fmt.Errorf("initial Product creation returned no identity")
	}
	_, err = service.ScanWorkspace(ctx, product.ID, "system/workspace")
	return err
}
