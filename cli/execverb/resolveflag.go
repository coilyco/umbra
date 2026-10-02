package execverb

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/coilyco/umbra/pkg/valuesource"
)

// filePrefix is the file-reference convention the wrapped CLIs already speak,
// and the form umbra hands back after resolving. See docs/execverb.md.
const filePrefix = "file://"

// resolvedValue is one rewritten flag: the temp file umbra wrote and the token
// it substituted, so the caller can unlink after the subprocess exits.
type resolvedValue struct{ path string }

// sourceFor maps a caller token onto a value source. A `<provider>://` prefix
// naming a registered provider selects it; anything else is a literal.
func sourceFor(token string, providers map[string]valuesource.Provider) valuesource.Source {
	scheme, rest, ok := strings.Cut(token, "://")
	if ok && providers[scheme] != nil {
		return valuesource.Source{Provider: scheme, Address: rest}
	}
	return valuesource.Source{Provider: "literal", Address: token}
}

// resolveFlagValues rewrites every declared resolve-flag in args: the caller's
// token resolves through valuesource, and the value is spilled to a 0600 file.
func resolveFlagValues(ctx context.Context, args []string, g Grant, providers map[string]valuesource.Provider) ([]string, []resolvedValue, error) {
	if len(g.ResolveFlags) == 0 {
		return args, nil, nil
	}
	out := append([]string{}, args...)
	var written []resolvedValue
	for _, flag := range g.ResolveFlags {
		i, joined, ok := findFlagValue(out, flag)
		if !ok {
			continue
		}
		token := out[i]
		if joined {
			token = token[len(flag)+1:]
		}
		rv, err := spillResolved(ctx, flag, token, providers)
		if err != nil {
			unlinkAll(written)
			return nil, nil, err
		}
		written = append(written, rv)
		if joined {
			out[i] = flag + "=" + filePrefix + rv.path
			continue
		}
		out[i] = filePrefix + rv.path
	}
	return out, written, nil
}

// spillResolved resolves one token and writes it to a private temp file, so the
// value reaches the subprocess by path and never enters argv.
func spillResolved(ctx context.Context, flag, token string, providers map[string]valuesource.Provider) (resolvedValue, error) {
	src := sourceFor(token, providers)
	value, err := valuesource.Resolve(ctx, providers, src.Provider, src.Address)
	if err != nil {
		// Names the provider and address, never the value.
		return resolvedValue{}, fmt.Errorf("resolve %s from %s %s: %w", flag, src.Provider, src.Address, err)
	}
	f, err := os.CreateTemp("", "umbra-value-*")
	if err != nil {
		return resolvedValue{}, fmt.Errorf("resolve %s: %w", flag, err)
	}
	path := f.Name()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return resolvedValue{}, fmt.Errorf("resolve %s: %w", flag, err)
	}
	if _, err := f.WriteString(value); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return resolvedValue{}, fmt.Errorf("resolve %s: %w", flag, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return resolvedValue{}, fmt.Errorf("resolve %s: %w", flag, err)
	}
	return resolvedValue{path: path}, nil
}

// findFlagValue locates the argv slot holding flag's value: the index of the
// separate token, or of the `--flag=value` token itself with joined true.
func findFlagValue(args []string, flag string) (idx int, joined, ok bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return i + 1, false, true
		}
		if strings.HasPrefix(a, flag+"=") {
			return i, true, true
		}
	}
	return 0, false, false
}

// unlinkAll removes every spilled value file. Best-effort: a failure to unlink
// must not mask the command's own outcome.
func unlinkAll(written []resolvedValue) {
	for _, w := range written {
		_ = os.Remove(w.path)
	}
}
