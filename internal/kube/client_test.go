package kube

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tlsClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(server.URL, filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt"), "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.HTTP.CloseIdleConnections)
	return client, server
}

func TestPaginationAndTokenRotation(t *testing.T) {
	var client *Client
	var calls atomic.Int32
	client, _ = tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		want := "Bearer rotated"
		if call == 1 {
			want = "Bearer first"
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("token was not reread at request %d", call)
		}
		if r.URL.Query().Get("limit") != "200" {
			t.Error("missing page bound")
		}
		metadata := map[string]string{"resourceVersion": "1"}
		if r.URL.Query().Get("continue") == "" {
			metadata["continue"] = "next"
		}
		if strings.HasSuffix(r.URL.Path, "/jobs") && r.URL.Query().Get("labelSelector") != Prefix+"pgbackrest-backup" {
			t.Error("job selector")
		}
		if strings.HasSuffix(r.URL.Path, "/cronjobs") && r.URL.Query().Get("labelSelector") != Prefix+"pgbackrest-backup=scheduled" {
			t.Error("cronjob selector")
		}
		if call == 1 {
			if err := os.WriteFile(client.TokenFile+".new", []byte("rotated"), 0600); err != nil {
				t.Error(err)
			}
			if err := os.Rename(client.TokenFile+".new", client.TokenFile); err != nil {
				t.Error(err)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": metadata, "items": []any{map[string]any{"metadata": map[string]string{"name": "page-object"}}}})
	})
	sample, err := client.Collect(t.Context())
	if err != nil || len(sample.Clusters) != 2 || len(sample.Jobs) != 2 || len(sample.CronJobs) != 2 || calls.Load() != 6 {
		t.Fatal(sample, calls.Load(), err)
	}
}

func TestPaginationFailureDiscardsPartialSample(t *testing.T) {
	for name, reply := range map[string]string{
		"loop":             `{"metadata":{"continue":"same","resourceVersion":"1"},"items":[]}`,
		"missing metadata": `{"items":[]}`,
		"null items":       `{"metadata":{},"items":null}`,
		"truncated":        `{"metadata":{},"items":[`,
		"wrong shape":      `{"metadata":{},"items":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(reply)) })
			if sample, err := client.Collect(t.Context()); err == nil || len(sample.Clusters) != 0 {
				t.Fatal("partial sample escaped", sample, err)
			}
		})
	}
	client, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/postgresclusters") {
			_, _ = w.Write([]byte(`{"metadata":{},"items":[{}]}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("sensitive-secret-body"))
	})
	if sample, err := client.Collect(t.Context()); err == nil || len(sample.Clusters) != 0 || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(sample, err)
	}
}

func TestRetryCancellationAndRedirect(t *testing.T) {
	var calls atomic.Int32
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"metadata":{},"items":[]}`))
	})
	if _, err := client.Collect(t.Context()); err != nil || calls.Load() != 5 {
		t.Fatal(calls.Load(), err)
	}
	blocked, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := blocked.Collect(ctx); err == nil || time.Since(start) > time.Second {
		t.Fatal("cancellation not bounded", err)
	}
	redirect, _ := tlsClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/credentials", http.StatusFound)
	})
	if _, err := redirect.Collect(t.Context()); err == nil {
		t.Fatal("redirect accepted")
	}
}

func TestPageBudgetAndChangedSnapshot(t *testing.T) {
	var page atomic.Int32
	client, _ := tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := page.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"continue": n, "resourceVersion": "1"}, "items": []any{}})
	})
	// Continuation fields must be strings, even on an otherwise empty response.
	if _, err := client.Collect(t.Context()); err == nil {
		t.Fatal("malformed metadata accepted")
	}
	client, _ = tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		n := page.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"continue": time.Unix(int64(n), 0).String(), "resourceVersion": "1"}, "items": []any{}})
	})
	page.Store(0)
	if _, err := client.Collect(t.Context()); err == nil || page.Load() != 100 {
		t.Fatal("page budget not enforced", page.Load(), err)
	}
	page.Store(0)
	client, _ = tlsClient(t, func(w http.ResponseWriter, _ *http.Request) {
		version := "1"
		if page.Add(1) > 1 {
			version = "2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"continue": "next", "resourceVersion": version}, "items": []any{}})
	})
	if _, err := client.Collect(t.Context()); err == nil {
		t.Fatal("changed snapshot accepted")
	}
}

func TestInvalidClientConfiguration(t *testing.T) {
	for _, origin := range []string{"http://localhost", "https://user:password@localhost", "https://localhost/path", "https://localhost?token=secret"} {
		if _, err := New(origin, "token", "ca", "test", time.Second); err == nil {
			t.Fatal(origin)
		}
	}
}
