// The paths-within gate: refuse a call whose path operands leave a set of roots
// that is only known at run time. See docs/execverb-paths-within.md.

package execverb

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const gatePathsWithin = "paths-within"

func init() { gateRegistry[gatePathsWithin] = buildPathsWithin }

// Operand grammars a paths-within gate can read. Each names a tool's argv shape,
// not a tool, so `rm` and `unlink`-like wraps share one.
const (
	operandsPositional = "positional"            // every non-flag word, `--` aware
	operandsWithTarget = "positional-and-target" // positional, plus mv's -t/--target-directory value
	operandsFind       = "find"                  // start paths and output files, only when a write predicate is present
)

// findWrites are the predicates that remove, rewrite or run something. A find
// without one only reads, so the gate lets it pass anywhere.
var findWrites = map[string]bool{
	"-delete": true, "-exec": true, "-execdir": true, "-ok": true, "-okdir": true,
	"-fprint": true, "-fprint0": true, "-fprintf": true, "-fls": true,
}

type pathsWithin struct {
	operands  string
	rootEnv   []string
	parentEnv []string
	fixed     []string
	reason    string
}

// buildPathsWithin reads the gate's options. Every misconfiguration fails the
// build: a gate that can never fire is worse than one that is absent.
func buildPathsWithin(gs GateSpec) (gateFunc, error) {
	if len(gs.Patterns) > 0 || len(gs.Allow) > 0 {
		return nil, fmt.Errorf("%s takes `operands`, `root`, `root-env`, `root-parent-env` and `reason`, not pattern or allow", gatePathsWithin)
	}
	g, err := pathsWithinOptions(gs.Options)
	if err != nil {
		return nil, err
	}
	switch g.operands {
	case operandsPositional, operandsWithTarget, operandsFind:
	case "":
		return nil, fmt.Errorf("%s needs `operands` (%s, %s or %s)", gatePathsWithin, operandsPositional, operandsWithTarget, operandsFind)
	default:
		return nil, fmt.Errorf("%s: operands %q is not %s, %s or %s", gatePathsWithin, g.operands, operandsPositional, operandsWithTarget, operandsFind)
	}
	if len(g.rootEnv)+len(g.parentEnv) == 0 {
		return nil, fmt.Errorf("%s needs `root-env` or `root-parent-env`: with neither, nothing could switch the gate on", gatePathsWithin)
	}
	return g.check, nil
}

func pathsWithinOptions(options map[string][]string) (pathsWithin, error) {
	var g pathsWithin
	for key, values := range options {
		switch key {
		case "operands", "reason":
			if len(values) != 1 {
				return g, fmt.Errorf("%s: %q expects exactly one value, got %d", gatePathsWithin, key, len(values))
			}
			if key == "operands" {
				g.operands = values[0]
			} else {
				g.reason = values[0]
			}
		case "root-env":
			g.rootEnv = values
		case "root-parent-env":
			g.parentEnv = values
		case "root":
			g.fixed = values
		default:
			return g, fmt.Errorf("%s: unknown node %q", gatePathsWithin, key)
		}
	}
	return g, nil
}

// check refuses the first operand outside the roots, and is inert while every
// named variable is empty (a shared PATH serves callers that never set them).
func (g pathsWithin) check(argv []string) error {
	roots, active := g.roots()
	if !active {
		return nil
	}
	for _, operand := range g.operandsOf(argv) {
		if len(roots) == 0 || !withinAny(resolveLeaf(operand), roots) {
			msg := fmt.Sprintf("%q is outside the directories this call may touch", operand)
			if g.reason != "" {
				msg += ": " + g.reason
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}

// roots resolves the allowed directories at call time. `/` and the home
// directory are dropped, so a wrongly set variable cannot mean "anywhere".
func (g pathsWithin) roots() (roots []string, active bool) {
	found, active := g.envRoots()
	if !active {
		return nil, false
	}
	uid := strconv.Itoa(os.Getuid())
	for _, r := range g.fixed {
		r = strings.ReplaceAll(r, "{uid}", uid)
		if info, err := os.Stat(r); err == nil && info.IsDir() {
			found = append(found, realpathLoose(r))
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		home = realpathLoose(home)
	}
	for _, r := range found {
		if r != string(filepath.Separator) && r != home {
			roots = append(roots, r)
		}
	}
	return roots, true
}

// envRoots reads the two environment-driven root sources.
func (g pathsWithin) envRoots() (found []string, active bool) {
	for _, name := range g.parentEnv {
		if v := os.Getenv(name); v != "" {
			active = true
			found = append(found, filepath.Dir(realpathLoose(v)))
		}
	}
	for _, name := range g.rootEnv {
		if v := os.Getenv(name); v != "" {
			active = true
			for _, r := range filepath.SplitList(v) {
				if r != "" {
					found = append(found, realpathLoose(r))
				}
			}
		}
	}
	return found, active
}

func (g pathsWithin) operandsOf(argv []string) []string {
	switch g.operands {
	case operandsFind:
		return findOperands(argv)
	case operandsWithTarget:
		return positionalOperands(argv, true)
	}
	return positionalOperands(argv, false)
}

// positionalOperands is every word that is not a flag, plus mv's target value
// when target is set. Spellings are in docs/execverb-paths-within.md.
func positionalOperands(argv []string, target bool) []string {
	var paths []string
	flagsDone := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case flagsDone || arg == "-" || !strings.HasPrefix(arg, "-"):
			paths = append(paths, arg)
		case arg == "--":
			flagsDone = true
		case target:
			var path string
			path, i = mvFlagValue(argv, i)
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// mvFlagValue reads the flag at argv[i] and returns the path it carries, if
// any, with the index of the last word it consumed.
func mvFlagValue(argv []string, i int) (path string, last int) {
	if strings.HasPrefix(argv[i], "--") {
		return mvLongFlag(argv, i)
	}
	return mvShortFlags(argv, i)
}

func mvLongFlag(argv []string, i int) (path string, last int) {
	name, value, hasValue := strings.Cut(argv[i][2:], "=")
	isTarget := name != "" && isPrefixOf(name, "target-directory")
	isSuffix := len(name) >= 2 && isPrefixOf(name, "suffix")
	if !isTarget && !isSuffix {
		return "", i
	}
	if !hasValue && i+1 < len(argv) {
		i++
		value = argv[i]
	}
	if isTarget {
		return value, i
	}
	return "", i
}

// mvShortFlags walks a cluster such as -vtDIR: the first t or S takes the
// rest of the word, or the next word, as its value.
func mvShortFlags(argv []string, i int) (path string, last int) {
	arg := argv[i]
	for j := 1; j < len(arg); j++ {
		if arg[j] != 't' && arg[j] != 'S' {
			continue
		}
		value := arg[j+1:]
		if value == "" && i+1 < len(argv) {
			i++
			value = argv[i]
		}
		if arg[j] == 't' {
			return value, i
		}
		return "", i
	}
	return "", i
}

func isPrefixOf(prefix, word string) bool { return strings.HasPrefix(word, prefix) }

// findOperands is empty for a find that only reads, else its start paths and
// the files an -fprint predicate names.
func findOperands(argv []string) []string {
	if !slices.ContainsFunc(argv, func(a string) bool { return findWrites[a] }) {
		return nil
	}
	i := skipFindOptions(argv)
	var paths []string
	for ; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == ")" || a == "," {
			break
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	for j := 0; j < len(argv)-1; j++ {
		switch argv[j] {
		case "-fprint", "-fprint0", "-fprintf", "-fls":
			paths = append(paths, argv[j+1])
		}
	}
	return paths
}

// skipFindOptions returns the index of the first word after find's leading
// -H, -L, -P, -D <debug> and -O<level> options.
func skipFindOptions(argv []string) int {
	i := 0
	for i < len(argv) {
		switch a := argv[i]; {
		case a == "-D":
			i += 2
		case a == "-H" || a == "-L" || a == "-P" || strings.HasPrefix(a, "-O"):
			i++
		default:
			return i
		}
	}
	return i
}

// resolveLeaf follows the parent's symlinks and leaves the leaf alone, since
// rm and mv act on a link, not its target.
func resolveLeaf(path string) string {
	full, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if strings.HasSuffix(path, "/") || full == string(filepath.Separator) {
		return realpathLoose(full)
	}
	return filepath.Join(realpathLoose(filepath.Dir(full)), filepath.Base(full))
}

// realpathLoose follows symlinks as far as the path exists and appends the rest
// unchanged, because a call may name something it is about to create.
func realpathLoose(path string) string {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(realpathLoose(parent), filepath.Base(path))
}

func withinAny(path string, roots []string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
