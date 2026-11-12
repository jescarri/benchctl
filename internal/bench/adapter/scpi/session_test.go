package scpi_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jescarri/benchctl/internal/bench/adapter/scpi"
	"github.com/jescarri/benchctl/internal/bench/adapter/scpi/scpitest"
	"github.com/jescarri/benchctl/internal/bench/domain"
)

func TestParseBlock(t *testing.T) {
	data := []byte{0, '#', '\n', 0xff}
	msg := append(append([]byte("C1:WF DAT2,"), scpi.FormatBlock(data)...), '\n', '\n')
	prefix, got, err := scpi.ParseBlock(msg)
	if err != nil || prefix != "C1:WF DAT2," || !bytes.Equal(got, data) {
		t.Fatalf("prefix=%q data=%v err=%v", prefix, got, err)
	}
	// A '#' inside the header text is skipped.
	_, got, err = scpi.ParseBlock([]byte("A#B,#13abc\n"))
	if err != nil || string(got) != "abc" {
		t.Fatalf("embedded hash: %q %v", got, err)
	}
	if _, _, err := scpi.ParseBlock([]byte("#9000000100short")); err == nil {
		t.Fatal("expected truncation error")
	}
	if _, _, err := scpi.ParseBlock([]byte("no block\n")); err == nil {
		t.Fatal("expected missing-block error")
	}
	_, got, err = scpi.ParseBlock([]byte("#0indef\n"))
	if err != nil || string(got) != "indef" {
		t.Fatalf("indefinite: %q %v", got, err)
	}
}

func TestParseSystErr(t *testing.T) {
	cases := map[string]scpi.InstrumentError{
		`-113,"Undefined header;FOO"`:   {Code: -113, Message: "Undefined header;FOO"},
		`+0, No error`:                  {Code: 0, Message: "No error"},
		`-113,Undefined header,C1:FOO `: {Code: -113, Message: "Undefined header,C1:FOO"},
		`0,"No error"`:                  {Code: 0, Message: "No error"},
	}
	for in, want := range cases {
		got, err := scpi.ParseSystErr(in)
		if err != nil || got != want {
			t.Errorf("%q → %+v %v", in, got, err)
		}
	}
	if _, err := scpi.ParseSystErr("garbage"); err == nil {
		t.Error("expected error")
	}
}

func TestSCPI1999CheckerDrainsQueue(t *testing.T) {
	queue := []string{`-113,"Undefined header;FOO"`, `-222,"Data out of range"`, `0,"No error"`}
	tr := scpitest.New(func(cmd string) (string, bool) {
		if cmd == "SYST:ERR?" {
			r := queue[0]
			if len(queue) > 1 {
				queue = queue[1:]
			}
			return r, true
		}
		return "", false
	})
	s := scpi.NewSession(tr, scpi.SCPI1999Checker)
	err := s.Set(context.Background(), "FOO")
	var list scpi.ErrorList
	if !errors.As(err, &list) || len(list) != 2 || list[1].Code != -222 {
		t.Fatalf("got %v", err)
	}
	if domain.KindOf(err) != domain.KindInstrument {
		t.Error("kind")
	}
	if err := s.Set(context.Background(), "OK"); err != nil {
		t.Fatalf("clean queue: %v", err)
	}
}

func TestSiglentLegacyChecker(t *testing.T) {
	esr := "32"
	tr := scpitest.New(scpitest.Map(map[string]string{"CMR?": "0", "EXR?": "21"}))
	tr.Handler = func(cmd string) (string, bool) {
		switch cmd {
		case "*ESR?":
			r := esr
			esr = "0"
			return r, true
		case "CMR?":
			return "0", true
		case "EXR?":
			return "21", true
		}
		return "", false
	}
	s := scpi.NewSession(tr, scpi.SiglentLegacyChecker)
	err := s.Set(context.Background(), "FOO")
	if err == nil || !strings.Contains(err.Error(), "command error") || !strings.Contains(err.Error(), "21,EXR") {
		t.Fatalf("got %v", err)
	}
	if err := s.Set(context.Background(), "C1:VDIV 1"); err != nil {
		t.Fatalf("clean ESR: %v", err)
	}
	if scpi.DescribeESR(scpi.ESRCME|scpi.ESREXE|scpi.ESRQYE|scpi.ESRDDE) != "command error, execution error, device-dependent error, query error" {
		t.Error("DescribeESR")
	}
}

func TestQueryHelpers(t *testing.T) {
	tr := scpitest.New(scpitest.Map(map[string]string{
		"C1:VDIV?": "2.00E+00V", "*OPC?": "1", "BAD?": "****",
		"BLK?": "DESC," + string(scpi.FormatBlock([]byte("xyz"))),
	}))
	s := scpi.NewSession(tr, nil)
	ctx := context.Background()
	if v, err := s.QueryFloat(ctx, "C1:VDIV?"); err != nil || v != 2 {
		t.Fatalf("QueryFloat %v %v", v, err)
	}
	if _, err := s.QueryFloat(ctx, "BAD?"); err == nil {
		t.Fatal("**** must not parse as a float")
	}
	if b, err := s.QueryBlock(ctx, "BLK?"); err != nil || string(b) != "xyz" {
		t.Fatalf("QueryBlock %q %v", b, err)
	}
	if err := s.WaitComplete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "UNKNOWN?"); err == nil {
		t.Fatal("expected no-response error")
	}
	if s.TransportName() != "fake" {
		t.Error("transport name")
	}
}

func TestFileLock(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	l1, err := scpi.AcquireFileLock(ctx, dir, "SN/1", 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = scpi.AcquireFileLock(ctx, dir, "SN/1", 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("expected busy, got %v", err)
	}
	if time.Since(start) < 100*time.Millisecond {
		t.Error("did not wait for the lock")
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := scpi.AcquireFileLock(ctx, dir, "SN/1", 0)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatal(err)
	}
	var nilLock *scpi.FileLock
	if nilLock.Release() != nil {
		t.Error("nil release")
	}
}

type deadlineProbe struct {
	scpitest.Transport
	sawDeadline time.Duration
}

func (d *deadlineProbe) Read(ctx context.Context) ([]byte, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.sawDeadline = time.Until(dl)
	}
	return []byte("1\n"), nil
}

func TestIOTimeout(t *testing.T) {
	p := &deadlineProbe{}
	tr := scpi.WithIOTimeout(p, 3*time.Second)
	_, _ = tr.Read(context.Background())
	if p.sawDeadline <= 2*time.Second || p.sawDeadline > 3*time.Second {
		t.Fatalf("default deadline %v", p.sawDeadline)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, _ = tr.Read(ctx)
	if p.sawDeadline < 50*time.Second {
		t.Fatalf("caller deadline must win, got %v", p.sawDeadline)
	}
	if scpi.WithIOTimeout(p, 0) != p {
		t.Error("zero timeout should not wrap")
	}
}

func TestTracerAndSummarize(t *testing.T) {
	var log bytes.Buffer
	tr := scpi.NewTracer(scpitest.New(scpitest.Map(map[string]string{"*IDN?": "A,B,C,D"})), "1.2.3.4", &log)
	s := scpi.NewSession(tr, nil)
	if _, err := s.Query(context.Background(), "*IDN?"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), `> "*IDN?\n"`) || !strings.Contains(log.String(), `< "A,B,C,D\n"`) {
		t.Fatalf("trace:\n%s", log.String())
	}
	if !strings.Contains(scpi.Summarize([]byte{0, 1, 2, 0xff}), "4 bytes binary") {
		t.Error("binary summary")
	}
}

func TestFileLockRefusesUnsafePaths(t *testing.T) {
	ctx := context.Background()
	// A symlinked lock file must not be followed (its target would be truncated).
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "SN1.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := scpi.AcquireFileLock(ctx, dir, "SN1", 0); err == nil {
		t.Fatal("followed a symlinked lock file")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep me" {
		t.Fatalf("target modified: %q", b)
	}
	// A group/world-writable lock directory is refused.
	open := t.TempDir()
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := scpi.AcquireFileLock(ctx, open, "SN1", 0); err == nil {
		t.Fatal("accepted a world-writable lock directory")
	}
	// A symlinked lock directory is refused.
	link := filepath.Join(t.TempDir(), "lockdir")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := scpi.AcquireFileLock(ctx, link, "SN1", 0); err == nil {
		t.Fatal("accepted a symlinked lock directory")
	}
}
