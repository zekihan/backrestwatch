package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zekihan/backrestwatch/internal/monitor"
)

func Handler(m *monitor.Monitor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		var payload any = monitor.Result{Reason: "Unknown path"}
		code := http.StatusNotFound
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			code, payload = http.StatusMethodNotAllowed, monitor.Result{Reason: "Method not allowed"}
		} else {
			switch {
			case r.URL.Path == "/healthz":
				code, payload = http.StatusOK, monitor.Result{Healthy: true}
			case r.URL.Path == "/backups":
				code, payload = m.Report("", time.Now())
			case strings.HasPrefix(r.URL.Path, "/backups/"):
				key := strings.TrimPrefix(r.URL.Path, "/backups/")
				if key != "" {
					code, payload = m.Report(key, time.Now())
				}
			}
		}
		w.WriteHeader(code)
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(payload)
		}
	})
}

// Serve owns both goroutines and drains HTTP before returning on cancellation.
func Serve(ctx context.Context, listener net.Listener, m *monitor.Monitor) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: Handler(m), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024}
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); m.Run(ctx) }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-serveDone:
	}
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()
	if shutdownErr := server.Shutdown(drainCtx); shutdownErr != nil {
		_ = server.Close()
		err = errors.Join(err, shutdownErr)
	}
	<-pollDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
