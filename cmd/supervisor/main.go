package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"releasecontrol/internal/supervisor"
)

func main() {
	defaultPath, err := supervisor.DefaultConfigPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve home directory")
		os.Exit(2)
	}
	configPath := flag.String("config", defaultPath, "supervisor JSON config path")
	flag.Parse()
	c, err := supervisor.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervisor config error:", err)
		os.Exit(2)
	}
	s, err := supervisor.New(c, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "supervisor setup error:", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = s.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "supervisor stopped:", err)
		os.Exit(1)
	}
}
