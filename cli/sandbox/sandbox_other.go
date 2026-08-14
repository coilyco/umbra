//go:build !linux

package sandbox

import "os/exec"

// Wrap is a no-op off Linux: OS-level jailing is unsupported, so the caller
// execs the real binary directly (pre-sandbox behavior).
func Wrap(_ *exec.Cmd, _ string, _ []string, _ *Spec) *Denial { return nil }

// Denial is the setup-denied channel; off Linux nothing is jailed, so it never
// fires. The type exists so callers compile identically on both platforms.
type Denial struct{}

// Fired is always false off Linux.
func (d *Denial) Fired() bool { return false }

// Close is a no-op off Linux.
func (d *Denial) Close() {}

// IsJailInvocation is always false off Linux.
func IsJailInvocation(_ []string) bool { return false }

// SetupDenied is always false off Linux: nothing was jailed, so there is no
// namespace-denial to degrade from.
func SetupDenied(_ *exec.Cmd, _ error) bool { return false }

// RunJail is unsupported off Linux.
func RunJail(_ []string) error {
	return errUnsupported
}

var errUnsupported = unsupportedError("sandbox: OS-level jail is Linux-only")

type unsupportedError string

func (e unsupportedError) Error() string { return string(e) }
