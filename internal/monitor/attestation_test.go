package monitor

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/zekihan/backrestwatch/internal/evidence"
	"github.com/zekihan/backrestwatch/internal/kube"
)

func TestUnauthenticatedMetadataCannotSupplySuccess(t *testing.T) {
	cluster := testCluster(t)
	cluster.Status.PGBackRest.ScheduledBackups = nil
	for _, kind := range []string{"ownerless Job", "forged cluster owner", "forged CronJob chain", "forged CronJob"} {
		t.Run(kind, func(t *testing.T) {
			job, cron := testJob(t), testCron(t)
			sample := kube.Sample{Clusters: []kube.Cluster{cluster}}
			switch kind {
			case "ownerless Job":
				job.Metadata.Owners = nil
				sample.Jobs = []kube.Job{job}
			case "forged cluster owner":
				sample.Jobs = []kube.Job{job}
			case "forged CronJob chain":
				job.Metadata.Labels[kube.Prefix+"pgbackrest-repo"] = "repo2"
				job.Metadata.Owners = []kube.Owner{{Kind: "CronJob", APIVersion: "batch/v1", Name: cron.Metadata.Name, UID: cron.Metadata.UID, Controller: true}}
				sample.Jobs, sample.CronJobs = []kube.Job{job}, []kube.CronJob{cron}
			case "forged CronJob":
				sample.CronJobs = []kube.CronJob{cron}
			}
			for _, keys := range []map[string]string{nil, testConfig().EvidencePublicKeys} {
				cfg := testConfig()
				cfg.EvidencePublicKeys = keys
				results, ledger, err := Evaluate(cfg, sample, nil, testNow)
				if err != nil {
					t.Fatal(err)
				}
				for key, result := range results {
					if result.Healthy || !ledger[key].LastSuccess.IsZero() {
						t.Fatal("untrusted metadata became durable success", results, ledger)
					}
				}
			}
		})
	}
}

func TestAttestationBindsBackupAndRejectsReplay(t *testing.T) {
	cluster := testCluster(t)
	cluster.Status.PGBackRest.ScheduledBackups = nil
	signed := attestFixture(kube.Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{testJob(t)}})
	cfg := testConfig()
	results, ledger, err := Evaluate(cfg, signed, nil, testNow)
	if err != nil || !results["db/db/repo1"].Healthy {
		t.Fatal("authentic backup rejected", results, err)
	}
	for name, mutate := range map[string]func(*kube.Sample){
		"object uid": func(s *kube.Sample) { s.Jobs[0].Metadata.UID = "copy" },
		"name":       func(s *kube.Sample) { s.Jobs[0].Metadata.Name = "copy" },
		"namespace":  func(s *kube.Sample) { s.Jobs[0].Metadata.Namespace = "other" },
		"completion": func(s *kube.Sample) { s.Jobs[0].Status.CompletionTime = "2026-10-02T11:01:00Z" },
		"creation":   func(s *kube.Sample) { s.Jobs[0].Metadata.CreationTimestamp = "2026-10-02T08:00:00Z" },
		"cluster uid": func(s *kube.Sample) {
			s.Clusters[0].Metadata.UID = "recreated"
			s.Jobs[0].Metadata.Owners[0].UID = "recreated"
		},
		"destination":          func(s *kube.Sample) { changeDestination(&s.Clusters[0]) },
		"ownerless signed job": func(s *kube.Sample) { s.Jobs[0].Metadata.Owners = nil },
		"signature": func(s *kube.Sample) {
			s.Jobs[0].Metadata.Annotations[attestationSignature] = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		},
		"unknown key": func(s *kube.Sample) { s.Jobs[0].Metadata.Annotations[attestationKey] = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			sample := attestFixture(kube.Sample{Clusters: []kube.Cluster{cluster}, Jobs: []kube.Job{testJob(t)}})
			mutate(&sample)
			results, next, err := Evaluate(cfg, sample, nil, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if results["db/db/repo1"].Healthy || !next["db/db/repo1"].LastSuccess.IsZero() {
				t.Fatal("copied attestation accepted", results)
			}
		})
	}
	// Authenticated evidence remains durable after object collection, but revoking
	// its public key discards the success without dropping destination barriers.
	store, err := evidence.Open(filepath.Join(t.TempDir(), "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(ledger); err != nil {
		t.Fatal(err)
	}
	ledger, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	results, ledger, err = Evaluate(cfg, kube.Sample{Clusters: []kube.Cluster{cluster}}, ledger, testNow.Add(time.Minute))
	if err != nil || !results["db/db/repo1"].Healthy {
		t.Fatal("authenticated checkpoint lost", results, err)
	}
	cfg.EvidencePublicKeys = nil
	results, ledger, err = Evaluate(cfg, kube.Sample{Clusters: []kube.Cluster{cluster}}, ledger, testNow.Add(2*time.Minute))
	if err != nil || results["db/db/repo1"].Healthy || !ledger["db/db/repo1"].LastSuccess.IsZero() || ledger["db/db/repo1"].NotBefore.IsZero() {
		t.Fatal("revoked key retained success", results, ledger, err)
	}
}

func TestLegacyUnsignedSuccessInvalidated(t *testing.T) {
	cluster := testCluster(t)
	cluster.Status.PGBackRest.ScheduledBackups = nil
	for _, source := range []string{"Completed Job", "CronJob lastSuccessfulTime", "unknown source"} {
		old := evidence.Ledger{"db/db/repo1": {UID: cluster.Metadata.UID, Created: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), RepositoryID: repositoryIdentity(cluster, "repo1"), LastSuccess: testNow.Add(-time.Hour), Source: source}}
		cfg := testConfig()
		results, next, err := Evaluate(cfg, kube.Sample{Clusters: []kube.Cluster{cluster}}, old, testNow)
		if err != nil || results["db/db/repo1"].Healthy || !next["db/db/repo1"].LastSuccess.IsZero() || !next["db/db/repo1"].NotBefore.Equal(testNow) {
			t.Fatal(results, next, err)
		}
	}
}
