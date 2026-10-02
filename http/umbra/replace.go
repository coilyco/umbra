// The replacement half of the driver: installing an occluded binary onto a shim
// directory, and reporting what that installation actually achieves.
// See docs/execverb-replacement.md.

package umbra

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/coilyco/umbra/cli/execverb"
)

// occluded returns the tool name this group's replacement stands in front of,
// empty when the group is an ordinary driver-tree binary.
func (g *group) occluded() string {
	for _, m := range g.Members {
		if m.Params.Replace {
			return m.Params.Occluded
		}
	}
	return ""
}

// wrappedBin returns the binary a replacement group wraps, empty otherwise.
func (g *group) wrappedBin() string {
	for _, m := range g.Members {
		if m.Params.Replace && m.ExecGF != nil {
			return m.ExecGF.Bin
		}
	}
	return ""
}

// Install is `build` with the destination filename fixed by policy: a
// replacement installed under any other name occludes nothing.
func Install(opts Options) error {
	if opts.ShimDir == "" {
		return fmt.Errorf("umbra: install needs --shim-dir, the PATH directory the replacement is installed onto")
	}
	binPath, g, err := materialize(opts)
	if err != nil {
		return err
	}
	name := g.occluded()
	if name == "" {
		return fmt.Errorf("umbra: binary %q declares no `replace`, so there is nothing to install as a replacement; use `umbra build`", g.runtimeBinary())
	}
	dest := filepath.Join(opts.ShimDir, executablePathForOS(runtime.GOOS, name))
	if err := os.MkdirAll(opts.ShimDir, 0o750); err != nil {
		return fmt.Errorf("umbra: create shim dir: %w", err)
	}
	if err := copyExecutable(binPath, dest); err != nil {
		return err
	}
	if err := emitSkill(g, opts.SkillsOut); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "umbra: installed %s as %s\n", g.runtimeBinary(), dest)
	fmt.Fprintf(os.Stderr, "umbra: put %s ahead of the real %s on PATH, then `umbra doctor --shim-dir %s`\n", opts.ShimDir, name, opts.ShimDir)
	return nil
}

// Finding is one line of the doctor's report: the check, what was found, and
// whether it holds.
type Finding struct {
	Check  string
	Detail string
	OK     bool
}

// Doctor reports what a replacement achieves on this host and changes nothing.
// Its last finding never passes. See docs/execverb-replacement.md.
func Doctor(opts Options) ([]Finding, error) {
	if opts.ShimDir == "" {
		return nil, fmt.Errorf("umbra: doctor needs --shim-dir, the PATH directory the replacement is installed onto")
	}
	g, err := loadGroup(opts)
	if err != nil {
		return nil, err
	}
	name := g.occluded()
	if name == "" {
		return nil, fmt.Errorf("umbra: binary %q declares no `replace`, so there is no occlusion to report on", g.runtimeBinary())
	}
	shim := filepath.Join(opts.ShimDir, executablePathForOS(runtime.GOOS, name))
	out := []Finding{installedFinding(shim), pathFinding(name, shim)}
	return append(out, reachFindings(g.wrappedBin(), opts.ShimDir)...), nil
}

// installedFinding reports whether the replacement is installed at all, the
// check every later one assumes.
func installedFinding(shim string) Finding {
	if _, err := os.Stat(shim); err != nil {
		return Finding{Check: "installed", Detail: fmt.Sprintf("no replacement at %s: run `umbra install --shim-dir`", shim)}
	}
	return Finding{Check: "installed", Detail: shim, OK: true}
}

// pathFinding reports what the tool's name resolves to now. A replacement that
// loses PATH order occludes nothing and says nothing.
func pathFinding(name, shim string) Finding {
	found, err := exec.LookPath(name)
	if err != nil {
		return Finding{Check: "wins on PATH", Detail: fmt.Sprintf("%q resolves to nothing on this PATH", name)}
	}
	if !sameFilePath(found, shim) {
		return Finding{Check: "wins on PATH", Detail: fmt.Sprintf("%q resolves to %s, not the replacement: put the shim dir earlier on PATH", name, found)}
	}
	return Finding{Check: "wins on PATH", Detail: fmt.Sprintf("%q resolves to the replacement", name), OK: true}
}

// reachFindings report where a granted call would go, and that the same binary
// is reachable without the replacement.
func reachFindings(bin, shimDir string) []Finding {
	if bin == "" {
		return []Finding{{Check: "real binary", Detail: "the guardfile names no wrapped binary"}}
	}
	resolved, err := execverb.ResolveRealExcluding(bin, shimDir)
	if err != nil {
		return []Finding{{Check: "real binary", Detail: fmt.Sprintf("no %q reachable outside the shim dir: a granted verb would refuse", bin)}}
	}
	return []Finding{
		{Check: "real binary", Detail: resolved, OK: true},
		{
			Check:  "still directly reachable",
			Detail: fmt.Sprintf("%s runs without passing through the replacement, so a PATH shim is what a caller sees rather than what a caller can reach", resolved),
		},
	}
}

// sameFilePath compares by identity rather than by string: one directory
// reaches PATH through symlinks and relative entries alike.
func sameFilePath(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// WriteFindings renders a doctor report, `ok` leading each line that holds.
func WriteFindings(w io.Writer, findings []Finding) {
	bad := 0
	for _, f := range findings {
		mark := "warn"
		if f.OK {
			mark = "ok  "
		} else {
			bad++
		}
		_, _ = fmt.Fprintf(w, "%s %-24s %s\n", mark, f.Check, f.Detail)
	}
	_, _ = fmt.Fprintf(w, "\n%d of %d checks hold. The last one never does, and says why.\n", len(findings)-bad, len(findings))
}
