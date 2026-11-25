package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/prodsigner"
	"syscall"
)

func main() { os.Exit(run()) }
func run() int {
	defaultPath := os.Getenv("PROD_SIGNER_CONFIG")
	if defaultPath == "" {
		defaultPath = "/etc/prod-signer/config.json"
	}
	filename := flag.String("config", defaultPath, "strict JSON configuration")
	mode := flag.String("mode", "flux", "execution mode: flux or promote")
	flag.Parse()
	summary := prodsigner.Summary{}
	code := 0
	f, e := os.Open(*filename)
	if e != nil {
		summary.Errors++
		return 1
	}
	c, e := prodsigner.DecodeMode(f, *mode)
	f.Close()
	if e != nil {
		summary.Errors++
		return 1
	}
	if os.Setenv("RCP_SECRET_DIR", filepath.Dir(c.Registry.CredentialsFile)) != nil {
		return 1
	}
	if *mode == "promote" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if e = prodsigner.Promote(ctx, c, imagesigning.New(), os.Stdout); e != nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "production promotion loop failed")
			return 1
		}
		return 0
	}
	defer func() { _ = json.NewEncoder(os.Stdout).Encode(summary) }()
	// The shared registry adapter confines credential reads to this secret directory.
	source, e := prodsigner.InCluster()
	if e != nil {
		summary.Errors++
		return 1
	}
	summary, e = prodsigner.Reconcile(context.Background(), c, source, imagesigning.New(), os.Stderr)
	if e != nil {
		fmt.Fprintln(os.Stderr, "production reconciliation failed")
		code = 1
	}
	if prodsigner.WriteMetrics(c.MetricsFile, summary) != nil {
		summary.Errors++
		code = 1
	}
	return code
}
