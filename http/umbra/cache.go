// Cache layout and staleness for the out-of-band materialized consumer binary,
// keyed by the Guardfile's location. See docs/specverb.md.
package umbra

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coilyco/umbra/pkg/config"
)

// cacheSubdir namespaces the specverb caches under the framework cache root, so
// they sit beside (not inside) any other umbra cache.
const cacheSubdir = "specverb"

// stampName is the per-cache staleness sentinel: the hashes the last build was
// keyed on, so run can skip a rebuild when nothing changed.
const stampName = ".stamp.json"

// cacheRoot returns <config.CacheDir>/specverb, the parent of every per-consumer
// materialized module.
func cacheRoot() string {
	return filepath.Join(config.CacheDir(), cacheSubdir)
}

// cacheKey derives the per-consumer cache key from the Guardfile's absolute
// path, so two repos that share a binary name never collide on disk.
func cacheKey(guardfilePath string) (string, error) {
	abs, err := filepath.Abs(guardfilePath)
	if err != nil {
		return "", fmt.Errorf("umbra: resolve guardfile path: %w", err)
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16], nil
}

// cacheKeyForGroup keys the cache by runtime name plus root-relative identities.
// Absolute locations are excluded, preserving a re-rooted tree's cache identity.
func cacheKeyForGroup(g *group) string {
	paths := make([]string, len(g.Members))
	for i, m := range g.Members {
		paths[i] = filepath.ToSlash(m.Path)
	}
	sort.Strings(paths)
	parts := append([]string{"binary=" + g.runtimeBinary()}, paths...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])[:16]
}

// cacheDirForGroup returns the materialized module dir for a merged binary.
func cacheDirForGroup(g *group) string {
	return filepath.Join(cacheRoot(), cacheKeyForGroup(g))
}

// stamp records the inputs the cached binary was built from. run rebuilds when
// any field drifts from the on-disk artifacts (or the binary is missing).
type stamp struct {
	GuardfileHash    string `json:"guardfileHash"`
	SpecLockHash     string `json:"specLockHash"`
	DepLockHash      string `json:"depLockHash"`
	GeneratorVersion string `json:"generatorVersion"`
	LDVersion        string `json:"ldVersion"` // version stamped via -ldflags; rebuilds when it drifts
	BuiltAt          string `json:"builtAt"`
}

// hashBytes is the content hash used for every staleness input.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashConcat hashes an ordered set of byte slices as one combined staleness
// input. Callers pass the slices in a deterministic (sorted) order.
func hashConcat(bss ...[]byte) string {
	h := sha256.New()
	for _, b := range bss {
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readStamp loads the cache stamp; a missing stamp is reported as not-found so
// the caller treats it as stale rather than an error.
func readStamp(dir string) (*stamp, bool) {
	b, err := os.ReadFile(filepath.Join(dir, stampName))
	if err != nil {
		return nil, false
	}
	var s stamp
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, false
	}
	return &s, true
}

// writeStamp persists the staleness inputs for the freshly built cache.
func writeStamp(dir string, s stamp) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("umbra: marshal stamp: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stampName), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("umbra: write stamp: %w", err)
	}
	return nil
}

// stale reports whether the cache at dir must be rebuilt. A local-replace dep
// lock is always stale; see docs/umbra-materialization.md.
func stale(dir, binaryPath string, dl *DepLock, want stamp) bool {
	if dl.hasLocalReplace() {
		return true
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return true
	}
	got, ok := readStamp(dir)
	if !ok {
		return true
	}
	return got.GuardfileHash != want.GuardfileHash ||
		got.SpecLockHash != want.SpecLockHash ||
		got.DepLockHash != want.DepLockHash ||
		got.GeneratorVersion != want.GeneratorVersion ||
		got.LDVersion != want.LDVersion
}
