package service

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/monitor"
)

const testToken = "synthetic-monitoring-token-for-local-tests-only"

func TestBackupAuthorizationAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(testToken + "\n")
	m := monitor.New(config.Default(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := Handler(m, path)
	for _, route := range []string{"/backups", "/backups/db/db/repo1", "/backups/unknown/path/repo1", "/backups/"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			for _, header := range []string{"", "Bearer wrong", "Basic " + testToken} {
				req := httptest.NewRequest(method, route, nil)
				req.Header.Set("Authorization", header)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal(route, method, response)
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatal("HEAD leaked body")
				}
				for _, field := range []string{"clusterUID", "repositories", "db/db", testToken} {
					if strings.Contains(response.Body.String(), field) {
						t.Fatal("unauthenticated metadata leak", response.Body.String())
					}
				}
			}
		}
	}
	request := func(route, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := request("/backups", testToken)
	var report monitor.Report
	if response.Code != 503 || json.Unmarshal(response.Body.Bytes(), &report) != nil || report.Scope == "" {
		t.Fatal("authorized client lost detailed status", response)
	}
	rotated := testToken + "-rotated"
	write(rotated)
	if request("/backups", testToken).Code != 401 || request("/backups", rotated).Code != 503 {
		t.Fatal("rotation did not take effect")
	}
	duplicate := httptest.NewRequest(http.MethodGet, "/backups", nil)
	duplicate.Header.Add("Authorization", "Bearer "+rotated)
	duplicate.Header.Add("Authorization", "Bearer "+rotated)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, duplicate)
	if recorder.Code != 401 {
		t.Fatal("duplicate credentials accepted")
	}
	for _, invalid := range []string{"", "short", strings.Repeat("x", 4097), testToken + "\n\n"} {
		write(invalid)
		if request("/backups", rotated).Code != 503 || request("/healthz", "").Code != 200 {
			t.Fatal("invalid credential did not fail closed or broke probes")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if request("/backups", rotated).Code != 503 || request("/healthz", "").Code != 200 {
		t.Fatal("missing credential did not fail closed")
	}
}
