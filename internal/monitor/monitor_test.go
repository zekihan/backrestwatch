package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

type fakeAPI struct {
	sample kube.Sample
	err    error
}

func (f *fakeAPI) Collect(context.Context) (kube.Sample, error) { return f.sample, f.err }

func newTestMonitor(t *testing.T, api *fakeAPI, path string) (*Monitor, *evidence.Store) {
	t.Helper()
	store, err := evidence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := New(testConfig(), api, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return m, store
}

func TestRestartRecoveryAndFailedCollection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	cluster := testCluster(t)
	now := time.Now().UTC()
	cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	api := &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{cluster}}}
	m, store := newTestMonitor(t, api, path)
	if code, _ := m.Report("", time.Now()); code != 503 {
		t.Fatal(code)
	}
	m.Refresh(t.Context())
	if code, _ := m.Report("", time.Now()); code != 200 {
		t.Fatal(code)
	}
	api.err = errors.New("sensitive response body and credentials")
	m.Refresh(t.Context())
	if code, payload := m.Report("", time.Now()); code != 503 || payload.(Report).Reason == api.err.Error() {
		t.Fatal(code, payload)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	cluster.Status.PGBackRest.ScheduledBackups = nil
	api.err, api.sample.Clusters = nil, []kube.Cluster{cluster}
	restarted, store := newTestMonitor(t, api, path)
	defer store.Close()
	if code, _ := restarted.Report("", time.Now()); code != 503 {
		t.Fatal("checkpoint alone must not mark a startup sample healthy")
	}
	restarted.Refresh(t.Context())
	if code, _ := restarted.Report("", time.Now()); code != 200 {
		t.Fatal("restart lost completion evidence", code)
	}
	api.sample.Clusters[0].Metadata.UID = "replacement"
	restarted.Refresh(t.Context())
	if code, _ := restarted.Report("", time.Now()); code != 503 {
		t.Fatal("restart evidence applied to replacement")
	}
}

func TestCachedAgeAndSampleExpire(t *testing.T) {
	api := &fakeAPI{}
	m, store := newTestMonitor(t, api, filepath.Join(t.TempDir(), "evidence.json"))
	defer store.Close()
	results, _ := evaluate(t, kube.Sample{Clusters: []kube.Cluster{testCluster(t)}}, nil, testNow.Add(3599*time.Second))
	m.results, m.sampled, m.error = results, time.Now(), ""
	if code, _ := m.Report("db/db/repo1", m.sampled.Add(2*time.Second)); code != 503 {
		t.Fatal("cached success stayed green", code)
	}
	if code, payload := m.Report("db/db/repo2", m.sampled.Add(2*time.Second)); code != 200 || *payload.(Result).AgeSeconds != 8*3600+1 {
		t.Fatal("age must update on every report", code, payload)
	}
	if code, _ := m.Report("", m.sampled.Add(181*time.Second)); code != 503 {
		t.Fatal("sample stayed green", code)
	}
	if code, _ := m.Report("db/db/repo4", m.sampled); code != 404 {
		t.Fatal(code)
	}
	if code, _ := m.Report("", m.sampled.Add(-time.Second)); code != 503 {
		t.Fatal("negative sample elapsed accepted")
	}
}

type failedStore struct{}

func (failedStore) Load() (evidence.Ledger, error) { return evidence.Ledger{}, nil }
func (failedStore) Save(evidence.Ledger) error     { return errors.New("disk full") }

func TestPersistenceFailureDoesNotPublishSuccess(t *testing.T) {
	m := New(testConfig(), &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{testCluster(t)}}}, failedStore{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Refresh(t.Context())
	if code, _ := m.Report("", time.Now()); code != 503 || !m.sampled.IsZero() {
		t.Fatal("failed checkpoint was published")
	}
}

func TestConcurrentRefreshAndReports(t *testing.T) {
	api := &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{testCluster(t)}}}
	m, store := newTestMonitor(t, api, filepath.Join(t.TempDir(), "evidence.json"))
	defer store.Close()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				m.Report("", time.Now())
				m.Report("db/db/repo1", time.Now())
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			m.Refresh(t.Context())
		}
	})
	wg.Wait()
}
