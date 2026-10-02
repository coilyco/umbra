package execverb

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/exitcode"
)

// The hazard in one name: a replacement resolving its own name off the PATH it
// just won finds itself. Every test here fabricates that PATH deliberately.
const fakeTool = "faketool"

// realTool writes an executable stub into dir. Windows has no exec bit, so the
// PATH tests are unix-only rather than subtly wrong.
func realTool(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, fakeTool)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// shimOf plants a symlink named faketool in dir pointing at the running test
// binary, standing in for a replacement installed under the tool's own name.
func shimOf(t *testing.T, dir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if err := os.Symlink(self, filepath.Join(dir, fakeTool)); err != nil {
		t.Fatalf("symlink shim: %v", err)
	}
}

func requireUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("PATH resolution here depends on the unix exec bit")
	}
}

func TestResolveRealSkipsACandidateThatIsTheWrapper(t *testing.T) {
	requireUnix(t)
	shimDir, realDir := t.TempDir(), t.TempDir()
	shimOf(t, shimDir)
	want := realTool(t, realDir)
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)

	got, err := ResolveReal(fakeTool)
	if err != nil {
		t.Fatalf("ResolveReal: %v", err)
	}
	if !sameFileByPath(t, got, want) {
		t.Errorf("resolved %q, want the real tool %q", got, want)
	}
}

func TestResolveRealRefusesWhenEveryCandidateIsTheWrapper(t *testing.T) {
	requireUnix(t)
	shimDir := t.TempDir()
	shimOf(t, shimDir)
	t.Setenv("PATH", shimDir)

	got, err := ResolveReal(fakeTool)
	if err == nil {
		t.Fatalf("want a refusal, resolved %q: execing that is a fork bomb", got)
	}
	// Not policy_denied: nothing about the call was refused, the host simply
	// holds no copy of the tool this wrapper occludes.
	if code := exitcode.Of(err); code != exitcode.Internal {
		t.Errorf("exit code = %d, want Internal (%d)", code, exitcode.Internal)
	}
	if !strings.Contains(err.Error(), fakeTool) {
		t.Errorf("refusal must name the tool it could not find: %v", err)
	}
}

func TestResolveRealSkipsTheWrappersOwnDirectory(t *testing.T) {
	requireUnix(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	selfDir := filepath.Dir(self)
	decoy := filepath.Join(selfDir, fakeTool)
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Skipf("cannot write beside the test binary: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(decoy) })

	realDir := t.TempDir()
	want := realTool(t, realDir)
	t.Setenv("PATH", selfDir+string(os.PathListSeparator)+realDir)

	got, err := ResolveReal(fakeTool)
	if err != nil {
		t.Fatalf("ResolveReal: %v", err)
	}
	// A replacement is installed under the name it occludes, so the real tool
	// of that name can never share the directory: skipping it is not a guess.
	if !sameFileByPath(t, got, want) {
		t.Errorf("resolved %q, want %q from outside the wrapper's own directory", got, want)
	}
}

func TestResolveRealRefusesAPinnedPathThatIsTheWrapper(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if _, err := ResolveReal(self); err == nil {
		t.Fatal("a pinned path resolving to the wrapper itself must be refused")
	}
}

func TestResolveRealAcceptsAPinnedPath(t *testing.T) {
	requireUnix(t)
	want := realTool(t, t.TempDir())
	got, err := ResolveReal(want)
	if err != nil {
		t.Fatalf("ResolveReal: %v", err)
	}
	if !sameFileByPath(t, got, want) {
		t.Errorf("resolved %q, want the pinned %q", got, want)
	}
}

func TestResolveRealRefusesAnEmptyName(t *testing.T) {
	if _, err := ResolveReal(""); err == nil {
		t.Fatal("an empty binary name must fail closed")
	}
}

// sameFileByPath compares two paths by identity rather than by string, since a
// temp dir on macOS reaches the same file through /var and /private/var alike.
func sameFileByPath(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %q: %v", a, err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %q: %v", b, err)
	}
	return os.SameFile(ai, bi)
}

// The chain hazard, observed rather than imagined: two PATH shims for one tool,
// each skipping only itself, find each other and loop forever.
func TestChildPathDropsTheReplacementsOwnDirectory(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	selfDir, other := filepath.Dir(self), t.TempDir()
	t.Setenv("PATH", selfDir+string(os.PathListSeparator)+other)

	got := childPath()
	if got == "" {
		t.Fatal("the wrapper's own directory is on PATH, so the child needs an override")
	}
	value := strings.TrimPrefix(got, "PATH=")
	entries := filepath.SplitList(value)
	if len(entries) != 1 || entries[0] != other {
		t.Errorf("child PATH = %q, want only %q", value, other)
	}
}

func TestChildPathIsEmptyWhenNothingWouldChange(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := childPath(); got != "" {
		t.Errorf("child PATH override = %q, want none when the wrapper's directory is not on PATH", got)
	}
}
