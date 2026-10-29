package monitor

import (
	"context"
	"encoding/json"
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

func TestReportSnapshotSurvivesRefresh(t *testing.T) {
	api := &fakeAPI{sample: kube.Sample{Clusters: []kube.Cluster{testCluster(t)}}}
	m, store := newTestMonitor(t, api, filepath.Join(t.TempDir(), "evidence.json"))
	defer store.Close()
	m.results, m.ledger = evaluate(t, api.sample, nil, testNow)
	m.sampled, m.evaluated, m.error = testNow, testNow, ""
	code, payload := m.Report("", testNow)
	if code != 200 {
		t.Fatal(code, payload)
	}
	before, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	// HTTP encodes the returned payload after Report releases its read lock.
	// Publish another sample before encoding that same payload again.
	m.Refresh(t.Context())
	if !m.sampled.After(testNow) {
		t.Fatal("refresh did not publish a newer sample")
	}
	after, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("refresh changed an already returned report:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestReportOwnsReturnedTimestamps(t *testing.T) {
	for _, key := range []string{"", "db/db/repo1"} {
		t.Run("key="+key, func(t *testing.T) {
			m := New(testConfig(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.results, m.ledger = evaluate(t, kube.Sample{Clusters: []kube.Cluster{testCluster(t)}}, nil, testNow)
			m.sampled, m.evaluated, m.error = testNow, testNow, ""
			_, payload := m.Report(key, testNow)
			var result Result
			if key == "" {
				report := payload.(Report)
				*report.SampledAt = testNow.Add(-time.Hour)
				result = report.Repositories["db/db/repo1"]
			} else {
				result = payload.(Result)
			}
			*result.LastSuccess = testNow.Add(time.Hour)
			*result.AgeSeconds = 0
			if code, next := m.Report("", testNow); code != 200 {
				t.Fatalf("mutating returned timestamps corrupted cached health: %d %+v", code, next)
			}
			if !m.sampled.Equal(testNow) || !m.results["db/db/repo1"].LastSuccess.Equal(m.ledger["db/db/repo1"].LastSuccess) || *m.results["db/db/repo1"].AgeSeconds != 7*3600 {
				t.Fatal("returned timestamp or age aliases cached state")
			}
		})
	}
}

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
	anchor := time.Now()
	m.results, m.sampled, m.evaluated, m.error = results, anchor, anchor, ""
	if code, _ := m.Report("db/db/repo1", m.sampled.Add(2*time.Second)); code != 503 {
		t.Fatal("cached success stayed green", code)
	}
	reportAt := m.sampled.Add(2 * time.Second)
	if code, payload := m.Report("db/db/repo2", reportAt); code != 200 || *payload.(Result).AgeSeconds != 8*3600+1 {
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

type delayedAPI struct {
	sample   kube.Sample
	started  chan struct{}
	release  chan struct{}
	returned time.Time
}

func (d *delayedAPI) Collect(context.Context) (kube.Sample, error) {
	close(d.started)
	<-d.release
	d.returned = time.Now()
	return d.sample, nil
}

func TestSampleTimeStartsBeforeCollectionWithoutDoubleCountingAge(t *testing.T) {
	cluster := testCluster(t)
	now := time.Now().UTC()
	cluster.Metadata.CreationTimestamp = now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
	}
	api := &delayedAPI{
		sample:  kube.Sample{Clusters: []kube.Cluster{cluster}},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	m, store := newTestMonitor(t, &fakeAPI{}, filepath.Join(t.TempDir(), "evidence.json"))
	defer store.Close()
	m.api = api
	done := make(chan struct{})
	go func() { m.Refresh(t.Context()); close(done) }()
	<-api.started
	time.Sleep(75 * time.Millisecond)
	close(api.release)
	<-done
	if !m.sampled.Before(api.returned) || !m.evaluated.After(api.returned) || m.evaluated.Sub(m.sampled) < 50*time.Millisecond {
		t.Fatal("sample timestamp did not include collection time", m.sampled, api.returned, m.evaluated)
	}
	baseAge := *m.results["db/db/repo1"].AgeSeconds
	if code, payload := m.Report("db/db/repo1", m.evaluated); code != 200 || *payload.(Result).AgeSeconds != baseAge {
		t.Fatal("collection time was counted twice in cached backup age", code, payload, baseAge)
	}
	expiredSample := m.sampled.Add(time.Duration(m.config.MaxSampleAgeSeconds)*time.Second + time.Second)
	if code, _ := m.Report("", expiredSample); code != 503 {
		t.Fatal("collection time was excluded from sample lifetime", code)
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

func TestCachedCompletionFailsClosedDuringClockRollback(t *testing.T) {
	cluster := testCluster(t)
	completed := testNow.Add(-5 * time.Second)
	for i := range cluster.Status.PGBackRest.ScheduledBackups {
		cluster.Status.PGBackRest.ScheduledBackups[i].CompletionTime = completed.Format(time.RFC3339Nano)
	}
	results, ledger := evaluate(t, kube.Sample{Clusters: []kube.Cluster{cluster}}, nil, testNow)
	m := New(testConfig(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Collection started before the successful completion was observed. A wall
	// clock rollback can make that completion future while the sample is fresh.
	m.results, m.ledger = results, ledger
	m.sampled, m.evaluated, m.error = testNow.Add(-10*time.Second), testNow, ""
	rollback := completed.Add(-time.Second)
	if code, payload := m.Report("db/db/repo1", rollback); code != 503 {
		t.Errorf("future cached completion reported healthy: %d %+v", code, payload)
	}
	if code, payload := m.Report("", rollback); code != 503 || payload.(Report).Repositories["db/db/repo1"].Healthy {
		t.Errorf("aggregate accepted future cached completion: %d %+v", code, payload)
	}
	// Reporting must preserve the validated checkpoint and recover when wall
	// time catches up, without another API collection or evidence rewrite.
	if !m.ledger["db/db/repo1"].LastSuccess.Equal(completed) || !m.results["db/db/repo1"].Healthy {
		t.Fatal("rollback report mutated retained evidence")
	}
	if code, payload := m.Report("", testNow); code != 200 {
		t.Fatalf("clock catch-up did not restore health: %d %+v", code, payload)
	}
}
