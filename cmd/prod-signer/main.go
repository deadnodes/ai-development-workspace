package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"releasecontrol/internal/imagesigning"
	"releasecontrol/internal/prodsigner"
)

func main() { os.Exit(run()) }
func run() int {
	defaultPath := os.Getenv("PROD_SIGNER_CONFIG")
	if defaultPath == "" {
		defaultPath = "/etc/prod-signer/config.json"
	}
	filename := flag.String("config", defaultPath, "strict JSON configuration")
	flag.Parse()
	summary := prodsigner.Summary{}
	code := 0
	defer func() { _ = json.NewEncoder(os.Stdout).Encode(summary) }()
	f, e := os.Open(*filename)
	if e != nil {
		summary.Errors++
		return 1
	}
	c, e := prodsigner.Decode(f)
	f.Close()
	if e != nil {
		summary.Errors++
		return 1
	}
	// The shared registry adapter confines credential reads to this secret directory.
	if os.Setenv("RCP_SECRET_DIR", filepath.Dir(c.Registry.CredentialsFile)) != nil {
		summary.Errors++
		return 1
	}
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
