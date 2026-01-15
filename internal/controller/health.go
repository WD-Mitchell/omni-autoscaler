package controller

import (
	"context"
	"net/http"
	"sync/atomic"
)

// HealthServer provides health check endpoints
type HealthServer struct {
	ready atomic.Bool
	srv   *http.Server
}

// NewHealthServer creates a new health server
func NewHealthServer(addr string) *HealthServer {
	hs := &HealthServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", hs.healthzHandler)
	mux.HandleFunc("/readyz", hs.readyzHandler)

	hs.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return hs
}

// Start starts the health server
func (hs *HealthServer) Start() error {
	return hs.srv.ListenAndServe()
}

// Shutdown gracefully shuts down the health server
func (hs *HealthServer) Shutdown(ctx context.Context) error {
	return hs.srv.Shutdown(ctx)
}

// SetReady sets the ready state
func (hs *HealthServer) SetReady(ready bool) {
	hs.ready.Store(ready)
}

func (hs *HealthServer) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (hs *HealthServer) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if hs.ready.Load() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not ready"))
	}
}
