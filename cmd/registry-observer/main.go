package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"releasecontrol/internal/registryobserver"
	"syscall"
)

func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, "registry_observation_failed")
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 3 || os.Args[1] != "run-once" {
		return fmt.Errorf("usage")
	}
	c, e := registryobserver.Load(os.Args[2])
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	l, e := registryobserver.RunOnce(ctx, c)
	if e != nil {
		return e
	}
	if l == nil {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "NO_JOB"})
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]string{"job_id": l.Job.ID, "status": l.Job.Status})
}
