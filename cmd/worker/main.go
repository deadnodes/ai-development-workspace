package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"releasecontrol/internal/workerconfig"
	"releasecontrol/internal/workerrunner"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
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
		return fmt.Errorf("usage: worker run-once OPERATOR_CONFIG.json | recipe-hash RECIPE.json")
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
