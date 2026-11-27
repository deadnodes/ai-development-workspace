// Package main provides the fixture service.
package main

import "net/http"

// Handler serves status.
type Handler struct{}

// Status reports readiness.
func (Handler) Status() {}

// Register adds routes.
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", func(http.ResponseWriter, *http.Request) {})
}

func main() {}
