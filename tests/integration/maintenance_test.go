//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/config"
	"github.com/zekihan/backrestwatch/internal/evidence"
)

func TestOfflineMaintenanceCommands(t *testing.T) {
	binary := os.Getenv("BACKRESTWATCH_BINARY")
	if binary == "" {
		t.Fatal("run make test/integration")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "evidence.json")
	candidate := filepath.Join(dir, "candidate.json")
	backup := filepath.Join(dir, "before.json")
	cfg := config.Default()
	cfg.StateFile = state
	cfg.Clusters = []config.Cluster{{Namespace: "db", Name: "db", Repos: map[string]config.Repository{"repo1": {}}}}
	configPath := filepath.Join(dir, "config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	entries := evidence.Ledger{"db/db/repo1": {UID: "active", Created: now.Add(-24 * time.Hour), RepositoryID: "active-destination", LastSuccess: now.Add(-time.Hour), Source: "PostgresCluster scheduled-backup status"}, "old/old/repo1": {UID: "retired", Created: now.Add(-24 * time.Hour), RepositoryID: "missing", NotBefore: now.Add(-time.Hour)}}
	store, err := evidence.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(entries); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runCheck := func(path string, wantSuccess bool) {
		t.Helper()
		command := exec.CommandContext(t.Context(), binary, "--config", configPath, "--check-checkpoint", path, "--api-url", "not-an-api-origin", "--token-file", filepath.Join(dir, "missing-token"))
		output, err := command.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("checkpoint check error %v: %s", err, output)
		}
		if wantSuccess && !strings.Contains(string(output), "no API sample was taken") {
			t.Fatal(string(output))
		}
	}
	runCheck(state, true)
	original, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("case variants cannot hide saved success", func(t *testing.T) {
		successState := filepath.Join(dir, "success.json")
		entry := entries["old/old/repo1"]
		entry.LastSuccess, entry.Source = now.Add(-30*time.Minute), "Completed Job"
		doc, err := json.Marshal(map[string]any{"version": 2, "entries": evidence.Ledger{"old/old/repo1": entry}})
		if err != nil {
			t.Fatal(err)
		}
		// encoding/json accepts these spellings when recovering evidence.
		text := strings.ReplaceAll(string(doc), `"lastSuccess"`, `"LASTSUCCESS"`)
		text = strings.ReplaceAll(text, `"source"`, `"SOURCE"`)
		if err := os.WriteFile(successState, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		runCheck(successState, true)
		successBackup, successOutput := filepath.Join(dir, "success-before.json"), filepath.Join(dir, "success-candidate.json")
		command := exec.CommandContext(t.Context(), "python3", "../../scripts/stage_checkpoint_retirement.py", "--checkpoint", successState, "--config", configPath, "--backup", successBackup, "--output", successOutput, "--retire", "old/old/repo1")
		if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "no saved success") {
			t.Fatalf("saved success was not rejected: %v %s", err, output)
		}
		for _, path := range []string{successBackup, successOutput} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("rejected retirement created %s: %v", path, err)
			}
		}
		unchanged, err := os.ReadFile(successState)
		if err != nil || string(unchanged) != text {
			t.Fatal("rejected retirement changed checkpoint", err)
		}
	})

	script, err := filepath.Abs("../../scripts/stage_checkpoint_retirement.py")
	if err != nil {
		t.Fatal(err)
	}
	stage := exec.CommandContext(t.Context(), "python3", script, "--checkpoint", state, "--config", configPath, "--backup", backup, "--output", candidate, "--retire", "old/old/repo1")
	if output, err := stage.CombinedOutput(); err != nil {
		t.Fatalf("stage: %v %s", err, output)
	}
	runCheck(backup, true)
	runCheck(candidate, true)
	unchanged, err := os.ReadFile(state)
	if err != nil || string(unchanged) != string(original) {
		t.Fatal("staging mutated original", err)
	}
	if err := os.Rename(candidate, state); err != nil {
		t.Fatal(err)
	}
	runCheck(state, true)
	// Missing and malformed files must fail instead of being treated as empty state.
	runCheck(filepath.Join(dir, "missing.json"), false)
	if err := os.WriteFile(candidate, []byte(`{"version":99,"entries":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	runCheck(candidate, false)
	// Restore the validated backup using a same-directory candidate.
	restored, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, restored, 0600); err != nil {
		t.Fatal(err)
	}
	runCheck(candidate, true)
	if err := os.Rename(candidate, state); err != nil {
		t.Fatal(err)
	}
	runCheck(state, true)
	restored, err = os.ReadFile(state)
	if err != nil || string(restored) != string(original) {
		t.Fatal("rollback changed bytes", err)
	}
}
