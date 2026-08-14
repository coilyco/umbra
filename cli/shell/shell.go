// Package shell is the argv-only exec wrapper. Every argument is passed as a
// distinct argv element. No shell string is ever composed. If a downstream
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/cli/sandbox"
)

// Resolver turns a binary name into an executable path. Pluggable so tests
// can avoid touching the real $PATH.
type Resolver func(bin string) (string, error)

// PathResolver is the default resolver. Looks the binary up on $PATH.
func PathResolver(bin string) (string, error) {
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("shell: %s not found on PATH: %w", bin, err)
	}
	return p, nil
}

// Runner executes subprocesses on behalf of consumer verbs. Build one in main
// with the default Resolver and plumb it through to verbs that need it.
type Runner struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// Resolve locates a binary. Defaults to PathResolver when nil.
	Resolve Resolver
	// Env, when non-nil, is appended to os.Environ() and passed to the
	// child as cmd.Env. Used by the egress-proxy plumbing to inject
	Env []string
	// Sandbox, when non-nil, jails the child (Linux) so it and its descendants
	// re-enter the gate before invoking a wrapped tool. nil = run directly.
	Sandbox *sandbox.Spec
}

// ErrEmptyBinary is returned when Exec or Capture is called with "".
var ErrEmptyBinary = errors.New("shell: bin is empty")

func (r *Runner) resolver() Resolver {
	if r.Resolve != nil {
		return r.Resolve
	}
	return PathResolver
}

// resolvePath resolves bin, preferring the jail's stashed real binary so the
// gate execs the tool itself rather than looping back into the shim.
func (r *Runner) resolvePath(bin string) (string, error) {
	if p := sandbox.RealBin(bin); p != "" {
		return p, nil
	}
	return r.resolver()(bin)
}

// Exec runs bin with argv, streaming stdin/stdout/stderr through the Runner's
// fields. Returns the underlying exec error on failure.
func (r *Runner) Exec(ctx context.Context, bin string, argv ...string) error {
	return r.execIn(ctx, "", bin, argv...)
}

// ExecIn is like Exec but runs the child with cmd.Dir set to dir. Used by
// the consumer's exec verb when the matched repo config lives in a direct child of cwd:
func (r *Runner) ExecIn(ctx context.Context, dir, bin string, argv ...string) error {
	return r.execIn(ctx, dir, bin, argv...)
}

func (r *Runner) execIn(ctx context.Context, dir, bin string, argv ...string) error {
	if bin == "" {
		return ErrEmptyBinary
	}
	path, err := r.resolvePath(bin)
	if err != nil {
		return err
	}
	build := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, path, argv...)
		cmd.Stdout = r.Stdout
		cmd.Stderr = r.Stderr
		cmd.Stdin = r.Stdin
		if dir != "" {
			cmd.Dir = dir
		}
		if r.Env != nil {
			cmd.Env = append(os.Environ(), r.Env...)
		}
		return cmd
	}
	cmd := build()
	var denial *sandbox.Denial
	if r.Sandbox != nil {
		// Linux + not-already-jailed: re-exec through the jail helper; else no-op.
		denial = sandbox.Wrap(cmd, path, argv, r.Sandbox)
		defer denial.Close()
	}
	runErr := cmd.Run()
	// Two denial shapes: the clone refused (child never started) and the child
	// started but setup was refused pre-exec. Both mean the tool never ran.
	if sandbox.SetupDenied(cmd, runErr) || (runErr != nil && denial.Fired()) {
		// The environment denied the jail (restricted shell / container without
		// userns). Degrade to an unsandboxed run rather than dying. See docs/sandbox.md.
		r.warnSandboxDenied()
		return build().Run()
	}
	return runErr
}

// sandboxDenyWarnOnce ensures the degrade warning prints at most once per
// process, not per exec (the reaper shells out several times).
var sandboxDenyWarnOnce sync.Once

// warnSandboxDenied emits the one-time degrade notice to the Runner's Stderr.
func (r *Runner) warnSandboxDenied() {
	sandboxDenyWarnOnce.Do(func() {
		if r.Stderr == nil {
			return
		}
		_, _ = fmt.Fprintf(r.Stderr, "umbra: namespace sandbox unavailable here; running tools unsandboxed (set %s=1 to skip the retry). See docs/sandbox.md.\n", sandbox.EnvNoSandbox)
	})
}

// Capture runs bin with argv and returns stdout as bytes. Stderr is forwarded
// to the Runner's Stderr (defaulting to discard if nil). Useful for verbs
func (r *Runner) Capture(ctx context.Context, bin string, argv ...string) ([]byte, error) {
	if bin == "" {
		return nil, ErrEmptyBinary
	}
	path, err := r.resolvePath(bin)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = r.Stderr
	cmd.Stdin = r.Stdin
	if r.Env != nil {
		cmd.Env = append(os.Environ(), r.Env...)
	}
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}
