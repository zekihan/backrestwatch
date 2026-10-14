package monitor

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

type recoveringStore struct {
	entries evidence.Ledger
	fail    bool
}

func (s *recoveringStore) Load() (evidence.Ledger, error) { return maps.Clone(s.entries), nil }
func (s *recoveringStore) Save(entries evidence.Ledger) error {
	if s.fail {
		return syscall.ENOSPC
	}
	s.entries = maps.Clone(entries)
	return nil
}
func persistenceFixture(t *testing.T) (*Monitor, *fakeAPI, *recoveringStore) {
	t.Helper()
	cluster := testCluster(t)
	now := time.Now().UTC()
	cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	api := &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{cluster}}}
	disk := &recoveringStore{entries: evidence.Ledger{}}
	m := New(testConfig(), api, disk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Refresh(t.Context())
	requireRepositoryStatus(t, m, 200)
	return m, api, disk
}
func requireRepositoryStatus(t *testing.T, m *Monitor, want int) {
	t.Helper()
	if code, result := m.Report("db/db/repo1", time.Now()); code != want {
		t.Fatalf("status %d, want %d: %+v", code, want, result)
	}
}

func TestInvalidationsSurviveFailedCheckpoints(t *testing.T) {
	for _, kind := range []string{"destination", "repository", "cluster"} {
		t.Run(kind, func(t *testing.T) {
			m, api, disk := persistenceFixture(t)
			original := api.sample.Clusters[0]
			persisted := maps.Clone(disk.entries)
			sampled := m.sampled
			disk.fail = true
			switch kind {
			case "destination":
				api.sample.Clusters[0].Spec.Backups.PGBackRest.Global = map[string]string{"repo1-path": "/changed"}
			case "repository":
				api.sample.Clusters[0].Spec.Backups.PGBackRest.Repos = api.sample.Clusters[0].Spec.Backups.PGBackRest.Repos[1:]
			case "cluster":
				api.sample.Clusters = nil
			}
			for range 3 {
				m.Refresh(t.Context())
				requireRepositoryStatus(t, m, 503)
				if !m.sampled.Equal(sampled) || !maps.Equal(disk.entries, persisted) {
					t.Fatal("failed write published or changed durable evidence")
				}
			}
			// Collection failure must not discard the pending invalidation either.
			api.err = errors.New("temporary API outage")
			m.Refresh(t.Context())
			api.err = nil
			// The original destination and retained old evidence reappear before recovery.
			api.sample.Clusters = []kube.Cluster{original}
			oldJob := testJob(t)
			oldJob.Metadata.CreationTimestamp = time.Now().Add(-2 * time.Hour).Format(time.RFC3339Nano)
			oldJob.Status.CompletionTime = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
			api.sample.Jobs = []kube.Job{oldJob}
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 503)
			disk.fail = false
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 503)
			entry := disk.entries["db/db/repo1"]
			if !entry.LastSuccess.IsZero() || entry.NotBefore.IsZero() {
				t.Fatal("old completion survived invalidation", entry)
			}
			// Recovery writes a valid barrier checkpoint, which also survives restart.
			realStore, err := evidence.Open(filepath.Join(t.TempDir(), "evidence.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer realStore.Close()
			if err := realStore.Save(disk.entries); err != nil {
				t.Fatal(err)
			}
			restarted := New(testConfig(), api, realStore, m.logger)
			restarted.Refresh(t.Context())
			requireRepositoryStatus(t, restarted, 503)
			// A backup created after the recovered barrier can make this destination healthy.
			job := testJob(t)
			birth := entry.NotBefore.Add(time.Nanosecond)
			job.Metadata.CreationTimestamp = birth.Format(time.RFC3339Nano)
			job.Status.CompletionTime = birth.Add(time.Nanosecond).Format(time.RFC3339Nano)
			api.sample.Jobs = []kube.Job{job}
			disk.fail = true
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 503)
			if !m.ledger["db/db/repo1"].LastSuccess.IsZero() {
				t.Fatal("retained unpersisted success")
			}
			api.sample.Jobs = nil
			disk.fail = false
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 503)
			api.sample.Jobs = []kube.Job{job}
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 200)
			api.sample.Jobs = nil
			m.Refresh(t.Context())
			requireRepositoryStatus(t, m, 200)
		})
	}
}

func TestFailedCheckpointPreservesOnlyCommittedSuccess(t *testing.T) {
	m, api, disk := persistenceFixture(t)
	old := disk.entries["db/db/repo1"]
	disk.fail = true
	api.sample.Clusters[0].Status.PGBackRest.ScheduledBackups[0].CompletionTime = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
	m.Refresh(t.Context())
	requireRepositoryStatus(t, m, 503)
	if m.ledger["db/db/repo1"] != old {
		t.Fatal("uncommitted success replaced checkpoint evidence")
	}
	api.sample.Clusters[0].Status.PGBackRest.ScheduledBackups = nil
	disk.fail = false
	m.Refresh(t.Context())
	requireRepositoryStatus(t, m, 200)
	if disk.entries["db/db/repo1"] != old {
		t.Fatal("uncommitted success persisted on recovery")
	}
}

func TestConcurrentReportsDuringPersistenceRecovery(t *testing.T) {
	m, api, disk := persistenceFixture(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				m.Report("", time.Now())
				m.Report("db/db/repo1", time.Now())
			}
		})
	}
	wg.Go(func() {
		for i := range 20 {
			disk.fail = i%2 == 0
			api.sample.Clusters[0].Spec.Backups.PGBackRest.Global = map[string]string{"repo1-path": time.Now().String()}
			m.Refresh(t.Context())
		}
	})
	wg.Wait()
	requireRepositoryStatus(t, m, 503)
}
