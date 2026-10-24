package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
	"strconv"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 4 && os.Args[1] == "inspect-cleanup" {
		v, e := workerrunner.InspectLocalCleanup(os.Args[2], os.Args[3])
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	if len(os.Args) == 6 && os.Args[1] == "reconcile-cleanup" {
		v, e := workerrunner.ReconcileLocalCleanup(os.Args[2], os.Args[3], os.Args[4], os.Args[5])
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(v)
	}
	if len(os.Args) == 6 && os.Args[1] == "prepare-rootfs" {
		max, e := strconv.ParseInt(os.Args[4], 10, 64)
		if e != nil {
			return e
		}
		count, e := strconv.Atoi(os.Args[5])
		if e != nil {
			return e
		}
		spec, e := workerrunner.NormalizeRuntimeRootFS(os.Args[2], os.Args[3], max, count)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(spec)
	}
	if len(os.Args) == 3 && os.Args[1] == "recipe-hash" {
		file, err := os.Open(os.Args[2])
		if err != nil {
			return err
		}
		defer file.Close()
		var recipe workerconfig.Recipe
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&recipe); err != nil {
			return err
		}
		if err = workerconfig.ValidateExecution(recipe); err != nil {
			return err
		}
		fmt.Println(workerconfig.RecipeHash(recipe))
		return nil
	}
	if len(os.Args) == 4 && os.Args[1] == "watchdog" {
		return workerrunner.Watchdog(os.Args[2], os.Args[3])
	}
	if len(os.Args) != 3 || os.Args[1] != "run-once" {
		return fmt.Errorf("usage: worker inspect-cleanup STATE_DIR OWNER | reconcile-cleanup STATE_DIR OWNER EXPECTED_HASH OPERATOR_REF | run-once OPERATOR_CONFIG.json | recipe-hash RECIPE.json | prepare-rootfs SELECTED_TREE NEW_ARCHIVE MAX_EXPANDED_BYTES MAX_ENTRIES")
	}
	cfg, err := workerrunner.Load(os.Args[2])
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, err := workerrunner.RunOne(ctx, cfg)
	if report != nil {
		_ = json.NewEncoder(os.Stdout).Encode(report)
	}
	return err
}
