package monitor

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

func TestExistingCheckpointSourcesRemainUsableAfterRestart(t *testing.T) {
	for _, source := range []string{"Completed Job", "CronJob lastSuccessfulTime"} {
		t.Run(source, func(t *testing.T) {
			now := time.Now().UTC()
			cluster := testCluster(t)
			cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
			cluster.Status.PGBackRest.ScheduledBackups = nil
			const key = "db/db/repo1"
			entry := evidence.Entry{UID: cluster.Metadata.UID, Created: now.Add(-24 * time.Hour), RepositoryID: repositoryIdentity(cluster, "repo1"), NotBefore: now.Add(-2 * time.Hour), LastSuccess: now.Add(-time.Hour), Source: source}
			store, err := evidence.Open(filepath.Join(t.TempDir(), "evidence.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Save(evidence.Ledger{key: entry}); err != nil {
				t.Fatal(err)
			}
			m := New(testConfig(), &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{cluster}}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if code, _ := m.Report(key, now); code != 503 {
				t.Fatal("checkpoint bypassed fresh API sample")
			}
			m.Refresh(t.Context())
			if code, result := m.Report(key, time.Now()); code != 200 {
				t.Fatal("existing completion source was invalidated", code, result)
			}
			recovered, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if recovered[key] != entry {
				t.Fatal("existing completion or safety boundary changed", recovered[key])
			}
		})
	}
}
