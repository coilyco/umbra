//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// jailArgv0 is the internal subcommand the consumer's main routes to RunJail
// before any normal CLI parsing.
const jailArgv0 = "__jail"
const jailStashPath = "/tmp/.cliguard-jail"

// jailSentinelByte is what the child writes to its ExtraFiles pipe when jail
// setup is denied. A dedicated fd cannot be forged by a wrapped tool's exit code.
const jailSentinelByte = 'D'

// jailSentinelFD is where Wrap's ExtraFiles entry lands in the child.
const jailSentinelFD = 3

// Denial is the parent half of the setup-denied channel. Fired reports whether
// the jail child signalled "setup denied, target never ran". Nil is safe.
type Denial struct{ r, w *os.File }

// Fired reports whether the jail child signalled a denial. It drops the
// parent's write end first, so a silent child reads EOF instead of blocking.
func (d *Denial) Fired() bool {
	if d == nil || d.r == nil {
		return false
	}
	if d.w != nil {
		_ = d.w.Close()
		d.w = nil
	}
	buf := make([]byte, 1)
	n, err := d.r.Read(buf)
	return err == nil && n == 1 && buf[0] == jailSentinelByte
}

// Close releases both ends. Safe on a nil Denial and safe to call twice.
func (d *Denial) Close() {
	if d == nil {
		return
	}
	if d.w != nil {
		_ = d.w.Close()
		d.w = nil
	}
	if d.r != nil {
		_ = d.r.Close()
		d.r = nil
	}
}

// Wrap rewrites cmd to re-exec the consumer binary as the jail helper in a fresh
// user+mount namespace. No-op when jailed, opted out (EnvNoSandbox), or spec nil.
func Wrap(cmd *exec.Cmd, realPath string, argv []string, spec *Spec) *Denial {
	if spec == nil || spec.SelfExe == "" || Jailed() || NoSandbox() {
		return nil
	}
	execTool := filepath.Base(realPath)
	tools := withTool(spec.Tools, execTool)

	helper := []string{jailArgv0}
	for _, t := range tools {
		helper = append(helper, "--tool", t)
	}
	helper = append(helper, "--exec", execTool, "--")
	helper = append(helper, argv...)

	cmd.Path = spec.SelfExe
	cmd.Args = append([]string{spec.SelfExe}, helper...)
	cmd.SysProcAttr = jailSysProcAttr()

	// The child reports a mode-2 denial (namespace made, setup refused) here.
	// Without it the child has already run, so SetupDenied cannot see the case.
	r, w, err := os.Pipe()
	if err != nil {
		return nil // no channel; mode-1 detection still applies
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, w)
	return &Denial{r: r, w: w}
}

// withTool returns tools guaranteed to contain t.
func withTool(tools []string, t string) []string {
	for _, x := range tools {
		if x == t {
			return tools
		}
	}
	return append(append([]string(nil), tools...), t)
}

// jailSysProcAttr clones a user+mount namespace with an identity uid map (brew
// refuses root); CAP_SYS_ADMIN rides through as an ambient cap for the mounts.
func jailSysProcAttr() *syscall.SysProcAttr {
	uid, gid := os.Getuid(), os.Getgid()
	return &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                []uintptr{unix.CAP_SYS_ADMIN},
	}
}

// IsJailInvocation reports whether argv is the internal jail-helper re-exec;
// the consumer's main calls RunJail when true.
func IsJailInvocation(argv []string) bool {
	return len(argv) > 1 && argv[1] == jailArgv0
}

// SetupDenied reports whether a jailed cmd failed to *start* on a clone denial
// (EPERM/EACCES, child never ran), so the caller may retry it. See docs/sandbox.md.
func SetupDenied(cmd *exec.Cmd, err error) bool {
	if err == nil || cmd == nil || !IsJailInvocation(cmd.Args) {
		return false
	}
	if cmd.ProcessState != nil {
		return false
	}
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// RunJail is the in-namespace helper: it shims the wrapped tools, installs the
// seccomp denylist, and execs the real tool (args is os.Args[2:]).
func RunJail(args []string) error {
	// Pin this OS thread: the thread that installs the non-TSYNC seccomp filter
	// must be the one that execs, which collapses the other Go threads.
	runtime.LockOSThread()
	// Close the sentinel on exec so the wrapped tool never inherits it and
	// cannot write a denial after we hand the process image over.
	sealSentinel()

	execTool, rest, err := jailSetup(args)
	if err != nil {
		// Every step below runs before execJailTarget, so the target has not
		// run. A permission denial is the environment refusing, not a bug.
		if isSetupDenial(err) {
			signalSetupDenied()
		}
		return err
	}
	return execJailTarget(execTool, rest)
}

// jailSetup performs every pre-exec step. It returns the target to exec, so by
// construction any error here means the wrapped tool never ran.
func jailSetup(args []string) (execTool string, rest []string, err error) {
	tools, execTool, rest, err := parseJailArgs(args)
	if err != nil {
		return "", nil, err
	}
	if execTool == "" {
		return "", nil, fmt.Errorf("sandbox: jail missing --exec target")
	}
	resolvedTools := resolveTools(tools)
	if err := prepareJailMounts(resolvedTools); err != nil {
		return "", nil, err
	}
	if err := installToolShims(resolvedTools, spec().SelfExe); err != nil {
		return "", nil, err
	}
	if err := setJailEnvironment(); err != nil {
		return "", nil, err
	}
	if err := clearAmbientCaps(); err != nil {
		return "", nil, err
	}
	if err := lockdownSyscalls(); err != nil {
		return "", nil, err
	}
	return execTool, rest, nil
}

// isSetupDenial reports whether err is the environment refusing the jail
// (EPERM/EACCES) rather than a malformed spec or a missing path, which must
// still fail loudly instead of silently dropping the boundary.
func isSetupDenial(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// sentinelFile returns the ExtraFiles sentinel the parent attached, or nil when
// the child was started without one (a consumer calling RunJail directly).
func sentinelFile() *os.File {
	if _, err := unix.FcntlInt(uintptr(jailSentinelFD), unix.F_GETFD, 0); err != nil {
		return nil
	}
	return os.NewFile(jailSentinelFD, "umbra-jail-sentinel")
}

// sealSentinel marks the sentinel close-on-exec. Without this the wrapped tool
// inherits the fd and could forge a denial after the boundary is handed over.
func sealSentinel() {
	if _, err := unix.FcntlInt(uintptr(jailSentinelFD), unix.F_GETFD, 0); err != nil {
		return
	}
	_, _ = unix.FcntlInt(uintptr(jailSentinelFD), unix.F_SETFD, unix.FD_CLOEXEC)
}

// signalSetupDenied tells the parent the jail was refused before exec. Failure
// to signal is not fatal: the parent then reports the original error.
func signalSetupDenied() {
	f := sentinelFile()
	if f == nil {
		return
	}
	_, _ = f.Write([]byte{jailSentinelByte})
}

type resolvedTool struct {
	tool      string
	canonical string
	realPath  string
	dir       string
	needsLink bool
}

// resolveTools snapshots the wrapped tools' PATH entries before any mount
// rewrite can hide them.
func resolveTools(tools []string) []resolvedTool {
	resolved := make([]resolvedTool, 0, len(tools))
	for _, tool := range tools {
		rt, ok := resolveTool(tool)
		if ok {
			resolved = append(resolved, rt)
		}
	}
	return resolved
}

// resolveTool records one wrapped tool's current PATH entry. Missing tools stay
// a no-op, matching the pre-refactor behavior.
func resolveTool(tool string) (resolvedTool, bool) {
	canonical, err := exec.LookPath(tool)
	if err != nil {
		// Tool not present on this host: nothing to shim.
		return resolvedTool{}, false
	}
	realPath := canonical
	if resolved, err := filepath.EvalSymlinks(canonical); err == nil {
		realPath = resolved
	}
	return resolvedTool{
		tool:      tool,
		canonical: canonical,
		realPath:  realPath,
		dir:       filepath.Dir(canonical),
		needsLink: realPath != canonical,
	}, true
}

// prepareJailMounts makes the mount namespace private and stages the real tools
// inside a private tmpfs before canonical dirs are masked.
func prepareJailMounts(resolvedTools []resolvedTool) error {
	// Make our mount view private so binds never propagate back to the host.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("sandbox: make-rprivate: %w", err)
	}

	// A private tmpfs stashes real binaries for the compiled-tool case, where
	// masking the canonical path would also hide the file the gate must exec.
	stash := jailStashPath
	if err := os.MkdirAll(stash, 0o700); err != nil {
		return fmt.Errorf("sandbox: mkdir stash: %w", err)
	}
	if err := unix.Mount("tmpfs", stash, "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("sandbox: mount stash tmpfs: %w", err)
	}

	mountedDirs := map[string]struct{}{}
	for _, tool := range resolvedTools {
		if err := stashRealBinary(tool.realPath, filepath.Join(stash, tool.tool)); err != nil {
			return err
		}
		if !tool.needsLink {
			continue
		}
		if _, seen := mountedDirs[tool.dir]; seen {
			continue
		}
		if err := mountPrivateToolDir(tool.dir); err != nil {
			return err
		}
		mountedDirs[tool.dir] = struct{}{}
	}
	return nil
}

// installToolShims binds the consumer shim into each wrapped tool's canonical
// location after the private staging area is ready.
func installToolShims(resolvedTools []resolvedTool, shim string) error {
	for _, tool := range resolvedTools {
		if err := installToolShim(tool, jailStashPath, shim); err != nil {
			return err
		}
	}
	return nil
}

func setJailEnvironment() error {
	if err := os.Setenv(EnvJailed, "1"); err != nil {
		return fmt.Errorf("sandbox: set jailed env: %w", err)
	}
	return nil
}

func clearAmbientCaps() error {
	// All bind-mounts are done; the tool itself must not keep CAP_SYS_ADMIN.
	// Clear the ambient set so the cap does not leak into the exec'd tool.
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("sandbox: clear ambient caps: %w", err)
	}
	return nil
}

func execJailTarget(execTool string, rest []string) error {
	target := os.Getenv(RealBinEnv(execTool))
	if target == "" {
		return fmt.Errorf("sandbox: exec target %q was not stashed (not on PATH?)", execTool)
	}
	fullArgv := append([]string{execTool}, rest...)
	// Replaces this image; target is our stashed real path, not user input.
	return syscall.Exec(target, fullArgv, os.Environ()) // #nosec G702 -- target is the resolved real binary, not user input
}

// installToolShim records one wrapped tool's real target for the gate to exec,
// then masks its canonical PATH entry with the consumer shim.
func installToolShim(tool resolvedTool, stash, shim string) error {
	stashed := filepath.Join(stash, tool.tool)
	realTarget := stashed
	if tool.needsLink {
		// Symlinked $0-sensitive tools need argv[0] to stay beside the canonical
		// entry, but the exec symlink itself must stay off the host filesystem.
		link := filepath.Join(tool.dir, "."+tool.tool+".cliguard")
		_ = os.Remove(link)
		if err := os.Symlink(stashed, link); err != nil {
			return fmt.Errorf("sandbox: symlink exec target %s: %w", tool.tool, err)
		}
		realTarget = link
	}
	if err := os.Setenv(RealBinEnv(tool.tool), realTarget); err != nil {
		return fmt.Errorf("sandbox: set realbin env %s: %w", tool.tool, err)
	}

	if err := touch(tool.canonical); err != nil {
		return fmt.Errorf("sandbox: ensure canonical entry %s: %w", tool.tool, err)
	}
	// Mask the canonical PATH/symlink entry with the shim (O_NOFOLLOW so the
	// bind lands on the symlink itself, leaving realTarget reachable).
	if err := maskWithShim(tool.canonical, shim); err != nil {
		return fmt.Errorf("sandbox: shim mask %s: %w", tool.tool, err)
	}
	return nil
}

// stashRealBinary copies the resolved tool into the private tmpfs stash so the
// jail can exec it after the canonical path is masked.
func stashRealBinary(realPath, stashed string) error {
	if err := touch(stashed); err != nil {
		return fmt.Errorf("sandbox: stash placeholder %s: %w", filepath.Base(stashed), err)
	}
	if err := unix.Mount(realPath, stashed, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("sandbox: stash bind %s: %w", filepath.Base(stashed), err)
	}
	return nil
}

// mountPrivateToolDir overlays a canonical tool directory with a private tmpfs
// so sandbox-only exec symlinks never land on the host filesystem.
func mountPrivateToolDir(dir string) error {
	if err := unix.Mount("tmpfs", dir, "tmpfs", 0, "mode=0700"); err != nil {
		return fmt.Errorf("sandbox: mount tool dir tmpfs %s: %w", dir, err)
	}
	return nil
}

// spec resolves the consumer binary (the jail helper itself) for the shim
// source; stable even after canonical tool paths are masked.
func spec() *Spec {
	exe, err := os.Executable()
	if err != nil {
		exe = "/proc/self/exe"
	}
	return &Spec{SelfExe: exe}
}

// parseJailArgs reads the `--tool <t>...  --exec <tool>  -- <argv...>` form.
func parseJailArgs(args []string) (tools []string, execTool string, rest []string, err error) {
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--tool":
			if i+1 >= len(args) {
				return nil, "", nil, fmt.Errorf("sandbox: --tool needs a value")
			}
			tools = append(tools, args[i+1])
			i += 2
		case "--exec":
			if i+1 >= len(args) {
				return nil, "", nil, fmt.Errorf("sandbox: --exec needs a value")
			}
			execTool = args[i+1]
			i += 2
		case "--":
			rest = args[i+1:]
			return tools, execTool, rest, nil
		default:
			return nil, "", nil, fmt.Errorf("sandbox: unexpected jail arg %q", args[i])
		}
	}
	return tools, execTool, rest, nil
}

// touch creates an empty file (the bind-mount target placeholder).
func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G302 -- placeholder, masked by bind-mount
	if err != nil {
		return err
	}
	return f.Close()
}

// maskWithShim bind-mounts shim over canonical via an O_PATH|O_NOFOLLOW fd at
// /proc/self/fd/N, so a symlink entry is masked without following to its target.
func maskWithShim(canonical, shim string) error {
	fd, err := unix.Open(canonical, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open canonical: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	target := "/proc/self/fd/" + strconv.Itoa(fd)
	if err := unix.Mount(shim, target, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind shim: %w", err)
	}
	return nil
}
