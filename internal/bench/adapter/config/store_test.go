package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jescarri/benchctl/internal/bench/domain"
)

func TestSaveFailureIsConfigError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a file, so MkdirAll fails.
	err := New(filepath.Join(blocker, "bench.json")).Save(&domain.Bench{})
	if domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("want usage (config) error, got %v", err)
	}
}

func TestLoadRejectsInvalidLimits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(p, []byte(`{"instruments":{},"limits":{"fg":{"max_amp_vpp":"5volts"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(p).Load(); domain.KindOf(err) != domain.KindUsage {
		t.Fatalf("want usage error, got %v", err)
	}
	s := New(p)
	if err := s.Save(&domain.Bench{Limits: map[string]map[string]any{"fg": {"max_amp_vpp": 10.0}}}); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Load(); err != nil || b.Limits["fg"]["max_amp_vpp"] != 10.0 {
		t.Fatalf("%+v %v", b, err)
	}
}
