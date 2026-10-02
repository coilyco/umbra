//go:build unix

package flock_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestExclusiveUnlockRoundTrip(t *testing.T) {
	f := openLock(t)
	if err := flock.Exclusive(f); err != nil {
		t.Fatalf("Exclusive: %v", err)
	}
	if err := flock.Unlock(f); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	// Re-taking after unlock must succeed on the same handle.
	if err := flock.Exclusive(f); err != nil {
		t.Fatalf("re-Exclusive after Unlock: %v", err)
	}
	if err := flock.Unlock(f); err != nil {
		t.Fatalf("final Unlock: %v", err)
	}
}

func TestUnlockWithoutLock(t *testing.T) {
	f := openLock(t)
	if err := flock.Unlock(f); err != nil {
		t.Fatalf("Unlock of unlocked file should be harmless, got %v", err)
	}
}

// A second handle must not acquire the lock until the first releases.
func TestExclusiveBlocksAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	defer func() { _ = first.Close() }()
	if err := flock.Exclusive(first); err != nil {
		t.Fatalf("Exclusive first: %v", err)
	}

	second, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer func() { _ = second.Close() }()

	got := make(chan error, 1)
	go func() { got <- flock.Exclusive(second) }()

	select {
	case err := <-got:
		t.Fatalf("second handle acquired the lock while first held it (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: the second acquire is still blocked.
	}

	if err := flock.Unlock(first); err != nil {
		t.Fatalf("Unlock first: %v", err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("second acquire after release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second handle did not acquire the lock after the first released")
	}
	_ = flock.Unlock(second)
}
