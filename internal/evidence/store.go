package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zekihan/backrestwatch/internal/config"
)

type Entry struct {
	UID         string    `json:"uid"`
	Created     time.Time `json:"created"`
	LastSuccess time.Time `json:"lastSuccess"`
	Source      string    `json:"source"`
}

type Ledger map[string]Entry

type document struct {
	Version int    `json:"version"`
	Entries Ledger `json:"entries"`
}

type Store struct {
	path string
	lock *os.File
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create evidence directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open evidence lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("evidence directory is already in use")
	}
	return &Store{path: path, lock: lock}, nil
}

func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) Load() (Ledger, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read evidence: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 || config.UniqueJSON(data) != nil {
		return nil, fmt.Errorf("invalid evidence file; preserve it and repair or restore it before retrying")
	}
	var doc document
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&doc) != nil || doc.Version != 1 || doc.Entries == nil || len(doc.Entries) > 4000 {
		return nil, fmt.Errorf("unsupported or corrupt evidence file")
	}
	for key, entry := range doc.Entries {
		if key == "" || entry.UID == "" || entry.Created.IsZero() || entry.LastSuccess.Before(entry.Created) || entry.Source == "" {
			return nil, fmt.Errorf("corrupt completion evidence")
		}
	}
	return doc.Entries, nil
}

func (s *Store) Save(entries Ledger) error {
	data, err := json.Marshal(document{Version: 1, Entries: entries})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".evidence-*")
	if err != nil {
		return fmt.Errorf("create evidence checkpoint: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("flush evidence checkpoint: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("replace evidence checkpoint: %w", err)
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
