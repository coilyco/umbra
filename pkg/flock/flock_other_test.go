//go:build !unix

package flock_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/flock"
)

// openLock creates and opens a lock file under t.TempDir().
func openLock(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// The property this pins is the whole of umbra#301: a caller must never read a
// no-op as a held lock, so nil is the one return that is wrong here.
func TestExclusiveRefusesOnNonUnix(t *testing.T) {
	err := flock.Exclusive(openLock(t))
	if err == nil {
		t.Fatal("Exclusive returned nil, which is indistinguishable from a held lock")
	}
	if !errors.Is(err, flock.ErrUnsupported) {
		t.Errorf("error is not ErrUnsupported, so a caller cannot tell it from contention: %v", err)
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("error does not name the platform: %v", err)
	}
}

// Unlock follows Exclusive, so the pair cannot drift into a state where taking
// a lock fails but releasing one appears to succeed.
func TestUnlockRefusesOnNonUnix(t *testing.T) {
	err := flock.Unlock(openLock(t))
	if err == nil {
		t.Fatal("Unlock returned nil while Exclusive refuses")
	}
	if !errors.Is(err, flock.ErrUnsupported) {
		t.Errorf("error is not ErrUnsupported: %v", err)
	}
}
