//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
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

	"github.com/zekihan/backrestwatch/internal/kube"
	"github.com/zekihan/backrestwatch/internal/monitor"
)

func TestProcessRestartRotationFailuresAndSignal(t *testing.T) {
	binary := os.Getenv("BACKRESTWATCH_BINARY")
	if binary == "" {
		t.Fatal("run make test/integration")
	}
	dir := t.TempDir()
	fixtureKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	const monitoringToken = "synthetic-process-monitoring-token-only"
	var mu sync.Mutex
	uid, token, evidencePresent, forbidden := "cluster-uid", "first", true, false
	bucket, jobCreated, jobCompleted := "old-bucket", "", ""
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
					"spec":   map[string]any{"backups": map[string]any{"pgbackrest": map[string]any{"repos": []any{map[string]any{"name": "repo1", "s3": map[string]string{"bucket": bucket, "endpoint": "s3.example.com", "region": "eu-west-1"}}}}}},
					"status": map[string]any{"pgbackrest": map[string]any{"scheduledBackups": backups}}}}
			}
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") && jobCreated != "" {
			cluster := kube.Cluster{}
			cluster.Metadata = kube.Metadata{UID: uid, CreationTimestamp: created}
			cluster.Spec.Backups.PGBackRest.Repos = []kube.Repository{{Name: "repo1", S3: map[string]string{"bucket": bucket, "endpoint": "s3.example.com", "region": "eu-west-1"}}}
			meta := kube.Metadata{Namespace: "db", Name: "new-backup", UID: "new-job", CreationTimestamp: jobCreated,
				Labels: map[string]string{kube.Prefix + "cluster": "db", kube.Prefix + "pgbackrest-repo": "repo1", kube.Prefix + "pgbackrest-backup": "scheduled"},
				Owners: []kube.Owner{{APIVersion: kube.ClusterAPI, Kind: "PostgresCluster", Name: "db", UID: uid, Controller: true}}}
			meta.Annotations = map[string]string{"backrestwatch.io/attestation-key": "fixture", "backrestwatch.io/attestation-signature": base64.StdEncoding.EncodeToString(ed25519.Sign(fixtureKey, monitor.AttestationMessage("Job", meta, cluster, "repo1", jobCompleted)))}
			items = []any{map[string]any{
				"metadata": meta,
				"status":   map[string]any{"succeeded": 1, "completionTime": jobCompleted, "conditions": []any{map[string]string{"type": "Complete", "status": "True"}}},
			}}
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
	write(filepath.Join(dir, "monitoring-token"), []byte(monitoringToken))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	config := map[string]any{"backupsTokenFile": filepath.Join(dir, "monitoring-token"), "evidencePublicKeys": map[string]string{"fixture": base64.StdEncoding.EncodeToString(fixtureKey.Public().(ed25519.PublicKey))}, "listenAddress": address, "stateFile": filepath.Join(dir, "evidence.json"), "pollIntervalSeconds": 1, "maxSampleAgeSeconds": 8, "requestTimeoutSeconds": 1, "collectionTimeoutSeconds": 3,
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
	wait := func(path string, code int, contains ...string) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			req, _ := http.NewRequest(http.MethodGet, "http://"+address+path, nil)
			req.Header.Set("Authorization", "Bearer "+monitoringToken)
			response, err := client.Do(req)
			if err == nil {
				body, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if strings.Contains(string(body), "private response") {
					t.Fatal("sensitive API body leaked")
				}
				if response.StatusCode == code && (len(contains) == 0 || strings.Contains(string(body), contains[0])) {
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
	for _, path := range []string{"/backups", "/backups/db/db/repo1", "/healthz"} {
		response, err := client.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		want := http.StatusUnauthorized
		if path == "/healthz" {
			want = http.StatusOK
		}
		if response.StatusCode != want || strings.Contains(string(body), "clusterUID") || strings.Contains(string(body), "repositories") {
			t.Fatal("unauthenticated route contract", path, response.StatusCode, string(body))
		}
	}
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
	// A same-name bucket change cannot reuse either disk or retained status.
	mu.Lock()
	bucket, evidencePresent = "new-bucket", true
	mu.Unlock()
	wait("/backups", 503, "No successful completed backup recorded")
	stop(cmd, done)
	cmd, done = start()
	wait("/backups", 503, "No successful completed backup recorded")
	mu.Lock()
	jobCreated = time.Now().UTC().Format(time.RFC3339Nano)
	jobCompleted = jobCreated
	mu.Unlock()
	wait("/backups", 200)
	stop(cmd, done)
	mu.Lock()
	jobCreated, evidencePresent = "", false
	mu.Unlock()
	cmd, done = start()
	wait("/backups", 200)
	mu.Lock()
	uid = "replaced-cluster"
	mu.Unlock()
	wait("/backups", 503)
	stop(cmd, done)
}
