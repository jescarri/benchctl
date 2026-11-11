//go:build bench

package bmp

import (
	"os"
	"testing"
)

// BMP_IN=... BMP_OUT=... go test -tags bench -run TestConvertFile ./internal/bench/adapter/bmp/
func TestConvertFile(t *testing.T) {
	in, out := os.Getenv("BMP_IN"), os.Getenv("BMP_OUT")
	if in == "" {
		t.Skip("BMP_IN not set")
	}
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ToPNG(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, p, 0o644); err != nil {
		t.Fatal(err)
	}
}
