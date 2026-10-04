//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProcessRestartRotationFailuresAndSignal(t *testing.T) {
	binary := os.Getenv("BACKRESTWATCH_BINARY")
	if binary == "" {
		t.Fatal("run make test/integration")
	}
	dir := t.TempDir()
	var mu sync.Mutex
	uid, token, evidencePresent, forbidden := "cluster-uid", "first", true, false
	created := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	completed := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if forbidden || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("private response must not appear in diagnostic endpoints"))
			return
		}
		items := []any{}
		metadata := map[string]string{"resourceVersion": "1"}
		if strings.HasSuffix(r.URL.Path, "/postgresclusters") {
			if r.URL.Query().Get("continue") == "" {
				metadata["continue"] = "last"
			} else {
				backups := []any{}
				if evidencePresent {
					backups = []any{map[string]any{"repo": "repo1", "succeeded": 1, "completionTime": completed}}
				}
				items = []any{map[string]any{"metadata": map[string]string{"namespace": "db", "name": "db", "uid": uid, "creationTimestamp": created},
					"spec":   map[string]any{"backups": map[string]any{"pgbackrest": map[string]any{"repos": []any{map[string]string{"name": "repo1"}}}}},
					"status": map[string]any{"pgbackrest": map[string]any{"scheduledBackups": backups}}}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "items": items})
	}))
	defer api.Close()
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	write(filepath.Join(dir, "token"), []byte("first"))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	config := map[string]any{"listenAddress": address, "stateFile": filepath.Join(dir, "evidence.json"), "pollIntervalSeconds": 1, "maxSampleAgeSeconds": 5, "requestTimeoutSeconds": 1, "collectionTimeoutSeconds": 3,
		"clusters": []any{map[string]any{"namespace": "db", "name": "db", "repos": map[string]any{"repo1": map[string]any{}}}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(dir, "config.json"), data)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := func() (*exec.Cmd, chan error) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "--config", filepath.Join(dir, "config.json"), "--api-url", api.URL, "--token-file", filepath.Join(dir, "token"), "--ca-file", filepath.Join(dir, "ca.crt"))
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd, done
	}
	stop := func(cmd *exec.Cmd, done chan error) {
		t.Helper()
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("process did not shut down gracefully")
		}
	}
	client := &http.Client{Timeout: time.Second}
	wait := func(path string, code int) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get("http://" + address + path)
			if err == nil {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if strings.Contains(string(body), "private response") {
					t.Fatal("sensitive API body leaked")
				}
				if response.StatusCode == code {
					return
				}
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		t.Fatalf("%s did not return %d", path, code)
	}
	cmd, done := start()
	wait("/backups", 200)
	stop(cmd, done)
	mu.Lock()
	evidencePresent = false
	mu.Unlock()
	cmd, done = start()
	wait("/backups/db/db/repo1", 200)
	mu.Lock()
	forbidden = true
	mu.Unlock()
	wait("/backups", 503)
	wait("/healthz", 200)
	wait("/backups/db/db/repo2", 404)
	mu.Lock()
	token = "rotated"
	forbidden = false
	write(filepath.Join(dir, "token.new"), []byte(token))
	if err := os.Rename(filepath.Join(dir, "token.new"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
	wait("/backups", 200)
	mu.Lock()
	uid = "replaced-cluster"
	mu.Unlock()
	wait("/backups", 503)
	stop(cmd, done)
}
