// Package config stores the bench inventory as JSON (bench.json).
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/jescarri/benchctl/internal/bench/domain"
	"github.com/jescarri/benchctl/internal/bench/port"
)

// FileStore implements port.ConfigStore.
type FileStore struct {
	path string
}

var _ port.ConfigStore = (*FileStore)(nil)

// DefaultPath returns ~/.config/benchctl/bench.json (XDG_CONFIG_HOME aware).
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "benchctl", "bench.json")
}

// New returns a store for path (DefaultPath if empty).
func New(path string) *FileStore {
	if path == "" {
		path = DefaultPath()
	}
	return &FileStore{path: path}
}

// Path implements port.ConfigStore.
func (s *FileStore) Path() string { return s.path }

// Load implements port.ConfigStore. A missing file yields an empty inventory.
func (s *FileStore) Load() (*domain.Bench, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &domain.Bench{Instruments: map[string]domain.InstrumentRef{}}, nil
	}
	if err != nil {
		return nil, domain.Usagef("read config: %v", err)
	}
	var bench domain.Bench
	if err := json.Unmarshal(b, &bench); err != nil {
		return nil, domain.Usagef("parse %s: %v", s.path, err)
	}
	if bench.Instruments == nil {
		bench.Instruments = map[string]domain.InstrumentRef{}
	}
	// Limits are validated by the operations that can energize an output
	// (App.benchForOutputs), never here: an invalid limit must not stop
	// `safe` from switching outputs off.
	return &bench, nil
}

// Save implements port.ConfigStore (atomic replace). Failures are config
// (usage) errors, like Load's.
func (s *FileStore) Save(b *domain.Bench) error {
	if err := s.save(b); err != nil {
		return domain.Usagef("save config %s: %v", s.path, err)
	}
	return nil
}

func (s *FileStore) save(b *domain.Bench) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".bench-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}
