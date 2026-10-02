// Real-binary resolution for an occluded replacement: find the tool this
// wrapper fronts without finding the wrapper. See docs/execverb-replacement.md.

package execverb

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// ResolveReal resolves bin to the real binary a replacement stands in front of,
// skipping the wrapper itself. Fails closed: no survivor means no command runs.
func ResolveReal(bin string) (string, error) {
	if bin == "" {
		return "", fmt.Errorf("execverb: cannot resolve an empty binary name (fail-closed)")
	}
	selfPath, selfInfo, err := selfIdentity()
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(bin, `/\`) {
		return resolvePinned(bin, selfInfo)
	}
	selfDir := filepath.Dir(selfPath)
	selfDirInfo, err := os.Stat(selfDir)
	if err != nil {
		return "", fmt.Errorf("execverb: cannot stat the replacement's own directory %q: %w (fail-closed)", selfDir, err)
	}
	return walkPath(bin, selfDirInfo, selfInfo)
}

// ResolveRealExcluding answers the doctor's question from outside the wrapper:
// given this shim directory, which binary would the shim reach?
func ResolveRealExcluding(bin, excludeDir string) (string, error) {
	if bin == "" {
		return "", fmt.Errorf("execverb: cannot resolve an empty binary name (fail-closed)")
	}
	if strings.ContainsAny(bin, `/\`) {
		return exec.LookPath(bin)
	}
	info, err := os.Stat(excludeDir)
	if err != nil {
		info = nil
	}
	return walkPath(bin, info, nil)
}

// selfIdentity resolves the running executable through symlinks and stats it. A
// wrapper that cannot identify itself cannot prove a candidate is not itself.
func selfIdentity() (string, os.FileInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("execverb: cannot identify the running executable, so a PATH candidate cannot be proven not to be it: %w (fail-closed)", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", nil, fmt.Errorf("execverb: cannot stat the running executable %q: %w (fail-closed)", exe, err)
	}
	return exe, info, nil
}

// resolvePinned handles a binary named with a path separator. The only hazard
// left is a pin pointing back at the wrapper through a symlink.
func resolvePinned(bin string, selfInfo os.FileInfo) (string, error) {
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("execverb: pinned binary %q is not executable: %w (fail-closed)", bin, err)
	}
	if sameFile(resolved, selfInfo) {
		return "", recursionError(bin, "the pinned path resolves to this replacement itself")
	}
	return resolved, nil
}

// walkPath returns the first PATH entry holding an executable bin that is in
// neither excludeDir nor selfInfo. Either may be nil, excluding nothing.
func walkPath(bin string, excludeDir, selfInfo os.FileInfo) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		if excludeDir != nil {
			if info, err := os.Stat(dir); err == nil && os.SameFile(info, excludeDir) { //nolint:gosec // compares identity only
				continue
			}
		}
		// LookPath on a path with a separator checks that one file, and still
		// applies PATHEXT on Windows, so the platform rules stay the stdlib's.
		candidate, err := exec.LookPath(filepath.Join(dir, bin))
		if err != nil {
			continue
		}
		if selfInfo != nil && sameFile(candidate, selfInfo) {
			continue
		}
		return candidate, nil
	}
	return "", recursionError(bin, "every PATH candidate was this replacement itself or absent")
}

// sameFile reports whether path is the same file as info, following symlinks so
// a link in another PATH directory cannot point back at the wrapper unnoticed.
func sameFile(path string, info os.FileInfo) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	candidate, err := os.Stat(path) //nolint:gosec // compares identity only
	if err != nil {
		return false
	}
	return os.SameFile(candidate, info)
}

// recursionError refuses rather than execing self. Internal rather than
// policy_denied: the call was not refused, the host holds no copy of the tool.
func recursionError(bin, why string) error {
	return exitcode.New(exitcode.Internal, "internal",
		fmt.Errorf("no real %q to run: %s", bin, why),
		fmt.Sprintf("install %q somewhere on PATH outside this replacement's own directory, or pin it with an absolute `exec` path", bin))
}

// occludeRunner resolves the real binary before each streamed exec, so a
// replacement never hands a bare name back to the PATH it just won.
func occludeRunner(run Runner) Runner {
	return func(ctx context.Context, bin string, argv, env []string) error {
		resolved, env, err := occlude(bin, env)
		if err != nil {
			return err
		}
		return run(ctx, resolved, argv, env)
	}
}

// occludeCapture is occludeRunner for the action path. Binding on only one of
// the two would be a hole rather than a guard.
func occludeCapture(capture CaptureRunner) CaptureRunner {
	return func(ctx context.Context, bin string, argv, env []string) ([]byte, []byte, int, error) {
		resolved, env, err := occlude(bin, env)
		if err != nil {
			return nil, nil, exitcode.Internal, err
		}
		return capture(ctx, resolved, argv, env)
	}
}

// Occluded is occlude for a caller outside the runner, such as the audit
// writer's git lookup: the real bin and the env entries to run it under.
func Occluded(bin string) (string, []string, error) { return occlude(bin, nil) }

// occlude resolves the real binary and the environment to run it under, shared
// by the streamed and captured paths.
func occlude(bin string, env []string) (string, []string, error) {
	resolved, err := ResolveReal(bin)
	if err != nil {
		return "", nil, err
	}
	if override := childPath(); override != "" {
		env = append(append([]string(nil), env...), override)
	}
	return resolved, env, nil
}

// childPath drops the replacement's own directory from the PATH its subprocess
// inherits. Resolution alone loops when a wrapped tool re-resolves its own name.
func childPath() string {
	raw, ok := os.LookupEnv("PATH")
	if !ok {
		return ""
	}
	selfPath, _, err := selfIdentity()
	if err != nil {
		return ""
	}
	selfDirInfo, err := os.Stat(filepath.Dir(selfPath))
	if err != nil {
		return ""
	}
	entries := filepath.SplitList(raw)
	kept := make([]string, 0, len(entries))
	for _, dir := range entries {
		probe := dir
		if probe == "" {
			probe = "."
		}
		if info, err := os.Stat(probe); err == nil && os.SameFile(info, selfDirInfo) { //nolint:gosec // compares identity only
			continue
		}
		kept = append(kept, dir)
	}
	if len(kept) == len(entries) {
		return ""
	}
	return "PATH=" + strings.Join(kept, string(os.PathListSeparator))
}
