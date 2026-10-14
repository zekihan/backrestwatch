package evidence

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckpointAndExclusiveOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "evidence.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("two writers acquired evidence lock")
	}
	if ledger, err := store.Load(); err != nil || len(ledger) != 0 {
		t.Fatal(ledger, err)
	}
	now := time.Now().UTC()
	want := Ledger{"db/db/repo1": {UID: "uid", RepositoryID: "destination", Created: now.Add(-time.Hour), LastSuccess: now, Source: "Completed Job"}}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Load()
	if err != nil || !got["db/db/repo1"].LastSuccess.Equal(now) {
		t.Fatal(got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	// An interrupted pre-rename write is never read as the committed checkpoint.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".evidence-interrupted"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptEvidenceFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, value := range []string{`{`, `{"version":3,"entries":{}}`, `{"version":1,"entries":null}`, `{"version":1,"version":1,"entries":{}}`, `{"version":1,"entries":{}} {}`, `{"version":1,"entries":{"x":{}}}`} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("corrupt evidence accepted", value)
		}
	}
}

func TestLegacyAndIdentityOnlyCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	legacy := `{"version":1,"entries":{"db/db/repo1":{"uid":"uid","created":"2026-01-01T00:00:00Z","lastSuccess":"2026-10-02T11:00:00Z","source":"Completed Job"}}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := store.Load()
	if err != nil || old["db/db/repo1"].RepositoryID != "" {
		t.Fatal(old, err)
	}
	birth := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := birth.Add(time.Hour)
	entry := Entry{UID: "uid", Created: birth, RepositoryID: "destination", NotBefore: cutoff}
	if err := store.Save(Ledger{"db/db/repo1": entry}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || !got["db/db/repo1"].NotBefore.Equal(cutoff) || !got["db/db/repo1"].LastSuccess.IsZero() {
		t.Fatal(got, err)
	}
	for _, mutate := range []func(*Entry){
		func(e *Entry) { e.RepositoryID = "" },
		func(e *Entry) { e.Source = "impossible success" },
		func(e *Entry) { e.NotBefore = birth.Add(-time.Second) },
		func(e *Entry) { e.LastSuccess = cutoff; e.Source = "Completed Job" },
		func(e *Entry) { e.LastSuccess = cutoff.Add(time.Second) },
	} {
		bad := entry
		mutate(&bad)
		if err := store.Save(Ledger{"db/db/repo1": bad}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("invalid identity checkpoint accepted", bad)
		}
	}
}
