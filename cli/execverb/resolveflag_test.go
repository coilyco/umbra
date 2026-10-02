package execverb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/pkg/valuesource"
)

// umbra#6830: two tokens stashed at 72 bytes stored at 71 only because a
// wrapper script stripped them first. The write path skipped valuesource.
func TestResolveFlagTrimsAFileSourcedValue(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "token")
	if err := os.WriteFile(src, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := Grant{Subcommand: []string{"put-parameter"}, ResolveFlags: []string{"--value"}}
	args := []string{"--name", "/x/y", "--value", "file://" + src}

	out, written, err := resolveFlagValues(context.Background(), args, g, valuesource.Merge(nil))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	defer unlinkAll(written)
	if len(written) != 1 {
		t.Fatalf("written = %d, want 1 spilled file", len(written))
	}
	got, err := os.ReadFile(written[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "s3cret" {
		t.Errorf("spilled %q, want the trailing newline trimmed by the file provider", string(got))
	}
	if out[3] != "file://"+written[0].path {
		t.Errorf("argv token = %q, want the path of the file umbra wrote", out[3])
	}
}

// The value must reach the subprocess by path on every input shape, so an
// inline literal is spilled too rather than left sitting in argv.
func TestResolveFlagSpillsAnInlineLiteralOffArgv(t *testing.T) {
	g := Grant{Subcommand: []string{"put-parameter"}, ResolveFlags: []string{"--value"}}
	for _, args := range [][]string{
		{"--value", "s3cret"},
		{"--value=s3cret"},
	} {
		out, written, err := resolveFlagValues(context.Background(), args, g, valuesource.Merge(nil))
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		for _, tok := range out {
			if strings.Contains(tok, "s3cret") {
				t.Errorf("%v: the value stayed in argv as %q", args, tok)
			}
		}
		got, err := os.ReadFile(written[0].path)
		if err != nil {
			t.Fatal(err)
		}
		// literal is deliberately not trimmed: valuesource.go:37.
		if string(got) != "s3cret" {
			t.Errorf("%v: spilled %q", args, string(got))
		}
		unlinkAll(written)
	}
}

func TestResolveFlagSpilledFileIsPrivateAndRemoved(t *testing.T) {
	g := Grant{Subcommand: []string{"put-parameter"}, ResolveFlags: []string{"--value"}}
	_, written, err := resolveFlagValues(context.Background(), []string{"--value", "x"}, g, valuesource.Merge(nil))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(written[0].path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("spilled file mode = %o, want 600", perm)
	}
	unlinkAll(written)
	if _, err := os.Stat(written[0].path); !os.IsNotExist(err) {
		t.Errorf("spilled file survived cleanup: %v", err)
	}
}

// An undeclared flag is untouched, so the node opts a verb in rather than
// changing what every other grant already does.
func TestResolveFlagLeavesUndeclaredFlagsAlone(t *testing.T) {
	g := Grant{Subcommand: []string{"put-parameter"}}
	args := []string{"--value", "file:///nope"}
	out, written, err := resolveFlagValues(context.Background(), args, g, valuesource.Merge(nil))
	if err != nil {
		t.Fatalf("no declared resolve-flag must be a no-op, got %v", err)
	}
	if len(written) != 0 || out[1] != "file:///nope" {
		t.Errorf("undeclared flag was rewritten: %v", out)
	}
}

// A source that cannot be read fails closed, and the error names the provider
// and address rather than any resolved bytes.
func TestResolveFlagFailsClosedOnAnUnreadableSource(t *testing.T) {
	g := Grant{Subcommand: []string{"put-parameter"}, ResolveFlags: []string{"--value"}}
	_, _, err := resolveFlagValues(context.Background(), []string{"--value", "file:///no/such/file"}, g, valuesource.Merge(nil))
	if err == nil {
		t.Fatal("an unreadable source must refuse before any exec")
	}
	if !strings.Contains(err.Error(), "/no/such/file") {
		t.Errorf("error should name the address it tried: %v", err)
	}
}

func TestResolveFlagReadsAnEnvSource(t *testing.T) {
	t.Setenv("UMBRA_TEST_SECRET", "from-env\n")
	g := Grant{Subcommand: []string{"put-parameter"}, ResolveFlags: []string{"--value"}}
	_, written, err := resolveFlagValues(context.Background(), []string{"--value", "env://UMBRA_TEST_SECRET"}, g, valuesource.Merge(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer unlinkAll(written)
	got, _ := os.ReadFile(written[0].path)
	if string(got) != "from-env" {
		t.Errorf("spilled %q, want the env provider's trim applied", string(got))
	}
}
