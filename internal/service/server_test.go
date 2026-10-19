package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
	"github.com/zekihan/backrestwatch/internal/monitor"
)

type blockedAPI struct{ started chan struct{} }

func (a blockedAPI) Collect(ctx context.Context) (kube.Sample, error) {
	close(a.started)
	<-ctx.Done()
	return kube.Sample{}, ctx.Err()
}

type blockedPersistence struct {
	started chan struct{}
	release chan struct{}
}

func (p blockedPersistence) Load() (evidence.Ledger, error) {
	close(p.started)
	<-p.release
	return evidence.Ledger{}, nil
}

func (blockedPersistence) Save(evidence.Ledger) error { return nil }

type observingAPI struct{ called chan struct{} }

func (a observingAPI) Collect(ctx context.Context) (kube.Sample, error) {
	close(a.called)
	return kube.Sample{}, ctx.Err()
}

func TestHTTPContractAndGracefulShutdown(t *testing.T) {
	cfg := config.Default()
	cfg.Clusters = []config.Cluster{{Namespace: "db", Name: "db", Repos: map[string]config.Repository{"repo1": {}}}}
	store, err := evidence.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := blockedAPI{started: make(chan struct{})}
	m := monitor.New(cfg, api, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, m) }()
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("collector did not start")
	}
	client := &http.Client{Timeout: time.Second}
	for path, code := range map[string]int{"/healthz": 200, "/backups": 503, "/backups/db/db/repo1": 503, "/backups/db/db/repo2": 404, "/backups/": 404, "/unknown": 404} {
		response, err := client.Get("http://" + listener.Addr().String() + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != code || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal(path, response.StatusCode)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	for method, code := range map[string]int{"HEAD": 200, "POST": 405} {
		recorder := httptest.NewRecorder()
		Handler(m).ServeHTTP(recorder, httptest.NewRequest(method, "/healthz", nil))
		if recorder.Code != code || (method == "HEAD" && recorder.Body.Len() != 0) {
			t.Fatal(method, recorder.Code)
		}
	}
	client.CloseIdleConnections()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(17 * time.Second):
		t.Fatal("shutdown leaked collector or HTTP server")
	}
}

func TestServeBoundsWaitForBlockedPersistence(t *testing.T) {
	var releaseOnce sync.Once
	release := make(chan struct{})
	letPersistenceFinish := func() { releaseOnce.Do(func() { close(release) }) }
	defer letPersistenceFinish()
	store := blockedPersistence{started: make(chan struct{}), release: release}
	api := observingAPI{called: make(chan struct{})}
	m := monitor.New(config.Default(), api, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, listener, m, 50*time.Millisecond) }()

	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("persistence Load did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return by its shutdown deadline")
	}

	// Releasing the persistence call lets the otherwise uninterruptible monitor
	// goroutine finish after Serve has returned.
	letPersistenceFinish()
	select {
	case <-api.called:
	case <-time.After(time.Second):
		t.Fatal("monitor did not continue after persistence was released")
	}
}
