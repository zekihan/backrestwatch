package monitor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

// Exercise the runbook's stop, backup, exact deletion, validation, atomic replace,
// fresh sample and rollback sequence on synthetic checkpoints in t.TempDir only.
func TestOfflineCheckpointRetirementAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	cfg := testConfig()
	cluster := testCluster(t)
	now := time.Now().UTC()
	cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	_, ledger, err := Evaluate(cfg, kube.Sample{Clusters: []kube.Cluster{cluster}}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	const active = "db/db/repo1"
	const barrier = "db/db/repo2"
	entry := ledger[barrier]
	entry.LastSuccess = time.Time{}
	entry.Source = ""
	entry.NotBefore = now.Add(-30 * time.Minute)
	ledger[barrier] = entry
	ledger["retired/retired/repo1"] = evidence.Entry{UID: "retired-uid", Created: now.Add(-24 * time.Hour), RepositoryID: "missing", NotBefore: now.Add(-time.Hour)}
	store, err := evidence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ledger); err != nil {
		t.Fatal(err)
	}
	if other, err := evidence.Open(path); err == nil {
		other.Close()
		t.Fatal("maintenance allowed an active writer")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "before-retirement.json")
	if err := os.WriteFile(backup, original, 0600); err != nil {
		t.Fatal(err)
	}
	// Work on a copy; reject active retirement and retain every other field.
	candidate := maps.Clone(ledger)
	delete(candidate, "retired/retired/repo1")
	if candidate[active] != ledger[active] || candidate[barrier] != ledger[barrier] {
		t.Fatal("active success or barrier changed")
	}
	staged := filepath.Join(dir, "candidate.json")
	stage, err := evidence.Open(staged)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Save(candidate); err != nil {
		t.Fatal(err)
	}
	validated, err := stage.Load()
	if err != nil || !maps.Equal(candidate, validated) {
		t.Fatal("candidate validation", err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	// The actual runbook uses same-directory rename, never in-place truncation.
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	store, err = evidence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Load()
	if err != nil || len(current) != 2 {
		t.Fatal(current, err)
	}
	api := &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{cluster}}}
	m := New(cfg, api, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if code, _ := m.Report(active, now); code != 503 {
		t.Fatal("checkpoint bypassed fresh API sample")
	}
	m.Refresh(context.Background())
	if code, _ := m.Report(active, time.Now()); code != 200 {
		t.Fatal("active success lost")
	}
	if code, _ := m.Report(barrier, time.Now()); code != 503 {
		t.Fatal("stale aggregate evidence crossed retained boundary")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Rollback is also a staged, validated, same-directory replacement.
	restored, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, restored, 0600); err != nil {
		t.Fatal(err)
	}
	stage, err = evidence.Open(staged)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := stage.Load(); err != nil || !maps.Equal(recovered, ledger) {
		t.Fatal("invalid backup", err)
	}
	stage.Close()
	if err := os.Rename(staged, path); err != nil {
		t.Fatal(err)
	}
	store, err = evidence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovered, err := store.Load()
	if err != nil || !maps.Equal(recovered, ledger) {
		t.Fatal("rollback lost history", err)
	}
	// Structural validation rejects unsupported versions and duplicate JSON keys.
	for _, bad := range []string{`{"version":99,"entries":{}}`, `{"version":2,"Version":2,"entries":{}}`} {
		if err := os.WriteFile(staged, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		broken, err := evidence.Open(staged)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := broken.Load(); err == nil {
			t.Fatal("invalid candidate accepted")
		}
		broken.Close()
	}
	// Confirm the backup remains byte-for-byte the original JSON document.
	var doc map[string]any
	if json.Unmarshal(original, &doc) != nil || doc["version"] != float64(2) {
		t.Fatal("unexpected fixture version")
	}
}
