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

func TestLoadKeepsInvalidLimits(t *testing.T) {
	// Load must not reject an invalid limit: safe needs the inventory to
	// switch outputs off. The operations that can energize an output
	// validate limits themselves (app.CheckGenLimits).
	p := filepath.Join(t.TempDir(), "bench.json")
	if err := os.WriteFile(p, []byte(`{"instruments":{},"limits":{"fg":{"max_amp_vpp":"5volts"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(p).Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ValidateLimits(); err == nil {
		t.Fatal("invalid limit not reported by ValidateLimits")
	}
}
