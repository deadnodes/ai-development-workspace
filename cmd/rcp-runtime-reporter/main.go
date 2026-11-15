package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"releasecontrol/internal/providers/runtimeobserver"
	"releasecontrol/internal/runtimereporter"
	"syscall"
	"time"
)

func main() {
	c, err := runtimereporter.Load(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	kube, err := runtimeobserver.NewInClusterGetClient()
	if err != nil {
		log.Fatal("invalid in-cluster configuration: ", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		cycle, done := context.WithTimeout(ctx, 10*time.Second)
		r, err := runtimereporter.Collect(cycle, kube, c)
		status := 0
		if err == nil {
			status, err = runtimereporter.Push(cycle, client, c, r)
		}
		done()
		delay := c.Interval
		if err != nil {
			if backoff == 0 {
				backoff = 30 * time.Second
			} else {
				backoff *= 2
			}
			if backoff > 5*time.Minute {
				backoff = 5 * time.Minute
			}
			delay = backoff
		} else {
			backoff = 0
		}
		log.Printf("runtime report workloads=%d namespaces=%d status=%d success=%t", len(r.Workloads), len(c.Namespaces), status, err == nil)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
