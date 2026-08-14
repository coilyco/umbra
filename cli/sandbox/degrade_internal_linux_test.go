//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// newTestDenial builds a Denial over a real pipe, as Wrap does.
func newTestDenial(t *testing.T) *Denial {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return &Denial{r: r, w: w}
}

// TestDenialFiresOnSentinel proves a child that signals is seen by the parent.
func TestDenialFiresOnSentinel(t *testing.T) {
	d := newTestDenial(t)
	if _, err := d.w.Write([]byte{jailSentinelByte}); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if !d.Fired() {
		t.Error("Fired() = false after the child wrote the sentinel")
	}
}

// TestDenialSilentChildDoesNotBlock is the regression that matters: a child
// that never signals must read EOF, not hang the parent forever.
func TestDenialSilentChildDoesNotBlock(t *testing.T) {
	d := newTestDenial(t)
	done := make(chan bool, 1)
	go func() { done <- d.Fired() }()
	select {
	case fired := <-done:
		if fired {
			t.Error("Fired() = true with no sentinel written")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fired() blocked on a silent child; the parent write end was not dropped")
	}
}

// TestDenialRejectsForeignBytes proves only the sentinel counts, so stray
// output on the fd cannot be mistaken for a denial.
func TestDenialRejectsForeignBytes(t *testing.T) {
	d := newTestDenial(t)
	if _, err := d.w.Write([]byte{'X'}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if d.Fired() {
		t.Error("Fired() = true for a non-sentinel byte")
	}
}

// TestDenialNilSafe proves the no-jail path (Wrap returning nil) is safe.
func TestDenialNilSafe(t *testing.T) {
	var d *Denial
	if d.Fired() {
		t.Error("nil Denial fired")
	}
	d.Close()
	d.Close()
}

// TestIsSetupDenial proves only permission denials degrade: a malformed spec or
// missing path must still fail loudly rather than silently drop the boundary.
func TestIsSetupDenial(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"eperm", syscall.EPERM, true},
		{"eacces", syscall.EACCES, true},
		{"wrapped eperm", fmt.Errorf("sandbox: make-rprivate: %w", syscall.EPERM), true},
		{"enoent", syscall.ENOENT, false},
		{"plain", fmt.Errorf("sandbox: jail missing --exec target"), false},
	} {
		if got := isSetupDenial(tc.err); got != tc.want {
			t.Errorf("%s: isSetupDenial = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWrapAttachesSentinel proves the parent always opens the channel, so a
// mode-2 denial has somewhere to be reported.
func TestWrapAttachesSentinel(t *testing.T) {
	t.Setenv(EnvNoSandbox, "")
	t.Setenv(EnvJailed, "")
	cmd := exec.Command("/bin/true")
	d := Wrap(cmd, "/bin/true", []string{"true"}, &Spec{SelfExe: "/proc/self/exe", Tools: []string{"true"}})
	if d == nil {
		t.Fatal("Wrap returned no Denial for a jailed command")
	}
	defer d.Close()
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %d, want 1 (the sentinel)", len(cmd.ExtraFiles))
	}
}
