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
	want := Ledger{"db/db/repo1": {UID: "uid", Created: now.Add(-time.Hour), LastSuccess: now, Source: "Completed Job"}}
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
	for _, value := range []string{`{`, `{"version":2,"entries":{}}`, `{"version":1,"entries":null}`, `{"version":1,"version":1,"entries":{}}`, `{"version":1,"entries":{}} {}`, `{"version":1,"entries":{"x":{}}}`} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("corrupt evidence accepted", value)
		}
	}
}
