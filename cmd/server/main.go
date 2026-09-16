package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"releasecontrol/internal/application"
	"releasecontrol/internal/persistence"
	"releasecontrol/internal/transport"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://releasecontrol:releasecontrol@localhost:55432/releasecontrol?sslmode=disable"
	}
	boot, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	store, err := persistence.Open(boot, dbURL)
	if err != nil {
		return err
	}
	defer store.Close()
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8090"
	}
	server := &http.Server{Addr: addr, Handler: transport.New(application.New(store), transport.Options{Token: os.Getenv("RC_TOKEN"), AllowedHosts: strings.FieldsFunc(os.Getenv("ALLOWED_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' })}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
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
