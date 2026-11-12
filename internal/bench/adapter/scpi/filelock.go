package scpi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/jescarri/benchctl/internal/bench/domain"
)

// FileLock is an exclusive advisory lock on one instrument (by serial), held
// for the lifetime of a benchctl process's connection.
type FileLock struct {
	f    *os.File
	path string
}

// LockDir returns $XDG_RUNTIME_DIR/benchctl, falling back to the temp dir.
func LockDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "benchctl")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("benchctl-%d", os.Getuid()))
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// AcquireFileLock takes the lock for serial in dir, polling until ctx is done
// or wait elapses. It fails fast with a clear error if another process holds it.
func AcquireFileLock(ctx context.Context, dir, serial string, wait time.Duration) (*FileLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkLockDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, unsafeChars.ReplaceAllString(serial, "_")+".lock")
	// O_NOFOLLOW: a planted symlink must not make us truncate its target.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file %s: %w", path, err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("lock file %s is not a regular file", path)
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = f.Truncate(0)
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			return &FileLock{f: f, path: path}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			holder, _ := os.ReadFile(path)
			_ = f.Close()
			return nil, domain.Instrumentf("instrument %s is busy: locked by another benchctl process (pid %s, %s)",
				serial, trimNL(holder), path)
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// checkLockDir refuses a lock directory another user could have prepared:
// LockDir's fallback (/tmp/benchctl-<uid>) has a predictable name in a shared
// directory. It must be a real directory (not a symlink), owned by us, not
// world-writable, and group-writable only for our own group.
func checkLockDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("lock directory %s is not a directory (symlink?)", dir)
	}
	perm := fi.Mode().Perm()
	if perm&0o002 != 0 {
		return fmt.Errorf("lock directory %s is world-writable (%v)", dir, perm)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) != os.Getuid() {
			return fmt.Errorf("lock directory %s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
		}
		// Group-writable is fine for the user's own (private) group only.
		if perm&0o020 != 0 && int(st.Gid) != os.Getgid() {
			return fmt.Errorf("lock directory %s is writable by group %d (%v)", dir, st.Gid, perm)
		}
	}
	return nil
}

func trimNL(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	if s == "" {
		return "?"
	}
	return s
}

// Release unlocks and closes the lock file.
func (l *FileLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
