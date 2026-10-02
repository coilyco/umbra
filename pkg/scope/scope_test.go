package scope_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/coilyco/umbra/pkg/config"
	"github.com/coilyco/umbra/pkg/scope"
)

// TestMain pins an app dir so the WARD_CACHE_DIR override the subtests set
// is honored and the toplevel cache lands in the tempdir, not real $HOME.
func TestMain(m *testing.M) {
	config.SetAppDir(".ward")
	os.Exit(m.Run())
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-C", dir, "config", "user.email", "test@example.com"},
		{"-C", dir, "config", "user.name", "test"},
	} {
		// `git init -q` runs inside dir; the rest use -C dir already.
		cmd := exec.Command("git", args...)
		if args[0] == "init" {
			cmd.Dir = dir
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Resolve symlinks (macOS /var → /private/var) so equality holds against
	// what `git rev-parse --show-toplevel` returns.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve symlinks: %v", err)
	}
	return resolved
}

func TestRepoRoot_InsideRepoReturnsToplevel(t *testing.T) {
	t.Setenv("WARD_CACHE_DIR", t.TempDir())
	dir := initRepo(t)
	if got := scope.RepoRoot(dir); got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestRepoRoot_InsideSubdirReturnsToplevel(t *testing.T) {
	t.Setenv("WARD_CACHE_DIR", t.TempDir())
	dir := initRepo(t)
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := scope.RepoRoot(sub); got != dir {
		t.Errorf("got %q, want %q (toplevel of subdir)", got, dir)
	}
}

func TestRepoRoot_OutsideRepoReturnsEmpty(t *testing.T) {
	t.Setenv("WARD_CACHE_DIR", t.TempDir())
	// t.TempDir() is not a git repo. RepoRoot is best-effort: empty, no error.
	if got := scope.RepoRoot(t.TempDir()); got != "" {
		t.Errorf("got %q, want empty for a non-repo cwd", got)
	}
}

// The regression that prompted the walk: an occluded `git` grants neither
// `rev-parse` nor a pre-verb `-C`, so shelling out lost the field silently.
func TestRepoRoot_SurvivesAGitReplacementOnPath(t *testing.T) {
	dir := initRepo(t)
	shim := t.TempDir()
	body := "#!/bin/sh\necho 'git: `git rev-parse` is not granted' >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(shim, "git"), []byte(body), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", shim)

	if got := scope.RepoRoot(dir); got != dir {
		t.Errorf("got %q, want %q: resolution must not depend on the PATH git", got, dir)
	}
}

// A worktree and a submodule carry .git as a file. git calls that a toplevel
// and so must this.
func TestRepoRoot_TreatsAGitFileAsAToplevel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere/.git/worktrees/x\n"), 0o600); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := scope.RepoRoot(dir); got != resolved {
		t.Errorf("got %q, want %q", got, resolved)
	}
}
