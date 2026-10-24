package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"releasecontrol/internal/ocipublisher"
	"releasecontrol/internal/publishrunner"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "run-once" {
		c, e := publishrunner.Load(os.Args[2])
		if e != nil {
			return e
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		out, e := publishrunner.RunOnce(ctx, c)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(os.Args) != 3 || os.Args[1] != "publish" {
		return fmt.Errorf("usage: publisher {publish|run-once} OPERATOR_CONFIG.json")
	}
	c, e := ocipublisher.Load(os.Args[2])
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	observation, e := ocipublisher.Publish(ctx, c)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(observation)
}
