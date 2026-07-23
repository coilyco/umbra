// The uv-style dependency lockfile (specverb.lock) and the `go` plumbing that
// resolves and replays it for a reproducible out-of-band build. See docs/specverb.md.
package kdlspecs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
)

// LockName is the committed dependency-lock filename, the uv.lock analog. It
// sits beside the Guardfile as source-of-truth; the generated Go does not.
const LockName = "specverb.lock"

// depLockVersion is the schema version of a LockName file. v2 made goMod a line
// array (symmetric with goSum), so the lock is diff-reviewable.
const depLockVersion = 2

// cliGuardModule is the framework's own module path. The driver is part of
// cli-guard, so it pins this module into every consumer's build.
const cliGuardModule = "forgejo.coilysiren.me/coilyco-flight-deck/cli-guard"

// buildModule is the throwaway local module path the cache dir builds under;
// it never resolves over the network, so the name is arbitrary but stable.
const buildModule = "specverbgen.local/build"

// buildGoDirective is the `go` directive seeded into the build module, tracking
// cli-guard's floor so the build never auto-downloads a toolchain; sync with go.mod.
const buildGoDirective = "1.25.5"

// DepLock is specverb.lock: the frozen build module. GoMod and GoSum are
// captured verbatim from a resolved build dir, reproducing the graph byte-for-byte.
type DepLock struct {
	Version  int      `json:"version"`  // schema version
	Go       string   `json:"go"`       // go directive of the build module
	CLIGuard string   `json:"cliGuard"` // resolved cli-guard module query (version, commit, or replace path)
	GoMod    []string `json:"goMod"`    // go.mod lines, order preserved
	GoSum    []string `json:"goSum"`    // sorted go.sum lines
}

// writeDepLock serializes dl to path as stable, indented JSON.
func writeDepLock(path string, dl *DepLock) error {
	dl.Version = depLockVersion
	sort.Strings(dl.GoSum)
	b, err := json.MarshalIndent(dl, "", "  ")
	if err != nil {
		return fmt.Errorf("kdl-specs: marshal %s: %w", LockName, err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil { //nolint:gosec // committed source-of-truth lock, not a secret
		return fmt.Errorf("kdl-specs: write %s: %w", path, err)
	}
	return nil
}

// readDepLock loads and version-checks a specverb.lock.
func readDepLock(path string) (*DepLock, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied lock path
	if err != nil {
		return nil, err
	}
	var dl DepLock
	if err := json.Unmarshal(b, &dl); err != nil {
		return nil, fmt.Errorf("kdl-specs: parse %s: %w", path, err)
	}
	if dl.Version != depLockVersion {
		return nil, fmt.Errorf("kdl-specs: %s schema version %d unsupported (want %d); re-run 'kdl-specs lock'", LockName, dl.Version, depLockVersion)
	}
	return &dl, nil
}

// buildVersion is stamped into release binaries. Go-installed binaries fall
// back to module build info, which already carries the installed tag.
var buildVersion string

// DriverVersion reports the installed kdl-specs version. Release binaries use
// their stamped tag, Go installs use build info, and source reports "(devel)".
func DriverVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// DefaultCLIGuardRef is the cli-guard module query a lock operation will freeze
// when the caller does not pass --cli-guard-ref.
func DefaultCLIGuardRef() string {
	return cliGuardVersion("")
}

// cliGuardVersion resolves the cli-guard module query to freeze into the lock:
// the explicit ref, else the driver's build version, else "latest" for a dev checkout.
func cliGuardVersion(ref string) string {
	if ref != "" {
		return ref
	}
	if v := DriverVersion(); v != "" && v != "(devel)" {
		return v
	}
	return "latest"
}

// goEnv returns the environment for `go` subprocesses: the inherited env plus
// GOPRIVATE so the Forgejo-hosted cli-guard module is fetched direct.
func goEnv() []string {
	return append(os.Environ(), "GOPRIVATE="+cliGuardModule)
}

// runGo runs `go args...` in dir, wrapping a non-zero exit with the combined
// output so callers surface the toolchain's own error.
func runGo(dir string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = goEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

// resolveDepLock runs `go mod tidy` in a seeded build dir and captures the
// resolved go.mod + go.sum. ref pins cli-guard; replace points it at a local checkout.
func resolveDepLock(dir, ref, replace string) (*DepLock, error) {
	cgVersion := cliGuardVersion(ref)
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n\nrequire %s %s\n", buildModule, buildGoDirective, cliGuardModule, modVersionForRequire(cgVersion, replace))
	if replace != "" {
		abs, err := filepath.Abs(replace)
		if err != nil {
			return nil, fmt.Errorf("kdl-specs: resolve cli-guard replace path: %w", err)
		}
		fmt.Fprintf(&b, "\nreplace %s => %s\n", cliGuardModule, abs)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(b.String()), 0o600); err != nil {
		return nil, fmt.Errorf("kdl-specs: seed build go.mod: %w", err)
	}
	if err := runGo(dir, "mod", "tidy"); err != nil {
		return nil, err
	}
	goMod, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // path under driver-owned temp dir
	if err != nil {
		return nil, fmt.Errorf("kdl-specs: read resolved go.mod: %w", err)
	}
	goSum, err := os.ReadFile(filepath.Join(dir, "go.sum")) //nolint:gosec // path under driver-owned temp dir
	if err != nil {
		return nil, fmt.Errorf("kdl-specs: read resolved go.sum: %w", err)
	}
	cg := cgVersion
	if replace != "" {
		cg = "replace=" + replace
	}
	return &DepLock{Go: buildGoDirective, CLIGuard: cg, GoMod: splitLines(goMod), GoSum: splitSortedLines(goSum)}, nil
}

// splitLines splits go.mod bytes into lines with order preserved (go.mod block
// structure is significant), dropping only the trailing newline.
func splitLines(b []byte) []string {
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// modVersionForRequire is the version token for the initial require line; a
// local replace gets the placeholder "v0.0.0" the replace directive overrides.
func modVersionForRequire(version, replace string) string {
	if replace != "" {
		return "v0.0.0"
	}
	return version
}

// splitSortedLines splits go.sum bytes into sorted, non-empty lines.
func splitSortedLines(b []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

// writeModuleFiles replays a DepLock's go.mod and go.sum into dir, the build
// step's reproducible input.
func writeModuleFiles(dir string, dl *DepLock) error {
	mod := strings.Join(dl.GoMod, "\n")
	if mod != "" {
		mod += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		return fmt.Errorf("kdl-specs: write build go.mod: %w", err)
	}
	sum := strings.Join(dl.GoSum, "\n")
	if sum != "" {
		sum += "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte(sum), 0o600); err != nil {
		return fmt.Errorf("kdl-specs: write build go.sum: %w", err)
	}
	return nil
}
