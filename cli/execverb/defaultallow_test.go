package execverb

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The shape default-allow exists for: one boundary named, the rest of the tool
// left alone rather than enumerated.
const defaultAllowGuardfile = `wrap gh {
    exec gh
    default-allow {
        reason "gh is a read tool with one boundary on it"
    }
    can run "api" { allow-flag "--cache" }
    withhold pr create {
        reason "pull requests go through Forgejo"
    }
}`

func TestDefaultAllowParsesItsReason(t *testing.T) {
	gf := parseOrFail(t, defaultAllowGuardfile)

	if !gf.DefaultAllow.Declared {
		t.Fatal("`default-allow` must mark the guardfile")
	}
	if !strings.Contains(gf.DefaultAllow.Reason, "read tool") {
		t.Errorf("reason = %q, want the authored sentence", gf.DefaultAllow.Reason)
	}
}

func TestDefaultAllowNeedsAReason(t *testing.T) {
	// Inverting the default is a decision, and an undefended one reads as an
	// accident to whoever opens the file next.
	_, err := Parse([]byte(`wrap gh {
    exec gh
    default-allow
    can run status
}`))
	if err == nil || !strings.Contains(err.Error(), "needs a `reason`") {
		t.Fatalf("err = %v, want a refusal naming the missing reason", err)
	}
}

func TestDefaultAllowRefusesUnknownChildren(t *testing.T) {
	_, err := Parse([]byte(`wrap gh {
    exec gh
    default-allow { rationale "wrong child" }
    can run status
}`))
	if err == nil || !strings.Contains(err.Error(), "unknown `default-allow` child") {
		t.Fatalf("err = %v, want a fail-closed refusal", err)
	}
}

func TestDefaultAllowCannotCoexistWithAWildcardFunnel(t *testing.T) {
	// Both answer the same call, and the funnel answers first, so the
	// declaration would name a default nothing reads.
	_, err := Parse([]byte(`wrap gh {
    exec gh
    default-allow { reason "x" }
    can run *
}`))
	if err == nil || !strings.Contains(err.Error(), "already forwards") {
		t.Fatalf("err = %v, want a refusal naming the funnel", err)
	}
}

func TestDefaultAllowNamingNothingIsPassthroughSpeltLong(t *testing.T) {
	_, err := Parse([]byte(`wrap gh {
    exec gh
    default-allow { reason "x" }
}`))
	if err == nil || !strings.Contains(err.Error(), "passthrough") {
		t.Fatalf("err = %v, want a refusal pointing at passthrough", err)
	}
}

func TestDefaultAllowMountsWithWithholdsAndNoGrants(t *testing.T) {
	// The whole point: a guardfile can name only what it refuses.
	gf := parseOrFail(t, `wrap gh {
    exec gh
    default-allow { reason "one boundary, no inventory" }
    withhold pr create { reason "pull requests go through Forgejo" }
}`)

	if len(gf.Grants) != 0 {
		t.Fatalf("grants = %d, want none", len(gf.Grants))
	}
	if len(gf.Withheld) != 1 {
		t.Fatalf("withheld = %d, want 1", len(gf.Withheld))
	}
}

func TestNoFallbackWithoutTheDeclaration(t *testing.T) {
	// nil is the refusal, so a guardfile that says nothing keeps fail-closed.
	gf := parseOrFail(t, replaceGuardfile)

	fb, err := NewFallback(Config{Guardfile: gf})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	if fb.Open() {
		t.Fatal("a guardfile declaring no default-allow must not forward")
	}
}

func TestForwardReachesTheRealBinaryWithTheCallersArgv(t *testing.T) {
	gf := parseOrFail(t, defaultAllowGuardfile)
	var gotBin string
	var gotArgv []string
	fb, err := NewFallback(Config{Guardfile: gf, Run: func(_ context.Context, bin string, argv, _ []string) error {
		gotBin, gotArgv = bin, argv
		return nil
	}})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}

	if err := fb.Forward(context.Background(), []string{"workflow", "run", "deploy.yml"}); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if gotBin != "gh" {
		t.Errorf("bin = %q, want gh", gotBin)
	}
	if strings.Join(gotArgv, " ") != "workflow run deploy.yml" {
		t.Errorf("argv = %v, want the caller's verbatim", gotArgv)
	}
}

func TestForwardStillFacesTheWrapLevelGuards(t *testing.T) {
	// Default-allow opens the verb surface, not the wrap's own gate, or
	// declaring a host guard goes decorative the moment the keyword appears.
	gf := parseOrFail(t, `wrap gh {
    exec gh
    default-allow { reason "one boundary" }
    only pass when hostname is build-box
    withhold pr create { reason "pull requests go through Forgejo" }
}`)
	fired := false
	fb, err := NewFallback(Config{
		Guardfile: gf,
		Run:       func(context.Context, string, []string, []string) error { fired = true; return nil },
		Host:      func(context.Context, []string) (string, error) { return "laptop", nil },
	})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}

	err = fb.Forward(context.Background(), []string{"workflow", "list"})

	if err == nil {
		t.Fatal("a wrap-level guard must refuse a forwarded call")
	}
	if fired {
		t.Error("the refusal has to happen before the binary is spawned")
	}
}

func TestForwardUnderReplaceResolvesPastTheShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the shim-resolution walk is POSIX-shaped here")
	}
	// A replacement wins PATH under the tool's own name, so a forward that
	// resolved a bare name would re-enter this process rather than the tool.
	dir := t.TempDir()
	realGh := filepath.Join(dir, "gh")
	if err := os.WriteFile(realGh, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("PATH", dir)

	gf := parseOrFail(t, `wrap gh {
    exec gh
    replace
    default-allow { reason "one boundary" }
    withhold pr create { reason "pull requests go through Forgejo" }
}`)
	var gotBin string
	fb, err := NewFallback(Config{Guardfile: gf, Run: func(_ context.Context, bin string, _, _ []string) error {
		gotBin = bin
		return nil
	}})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}

	if err := fb.Forward(context.Background(), []string{"repo", "view"}); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if gotBin != realGh {
		t.Errorf("bin = %q, want the resolved real binary %q", gotBin, realGh)
	}
}

func TestAnUnnamedSiblingUnderANamedGroupKeepsItsFlags(t *testing.T) {
	// A `pr` group parses its own flags before reaching CommandNotFound, so
	// `gh pr view 12 --json title` died on `--json` instead of forwarding.
	gf := parseOrFail(t, `wrap gh {
    exec gh
    default-allow { reason "one boundary" }
    withhold pr create { reason "pull requests go through Forgejo" }
}`)
	var gotArgv []string
	fb, err := NewFallback(Config{Guardfile: gf, Run: func(_ context.Context, _ string, argv, _ []string) error {
		gotArgv = argv
		return nil
	}})
	if err != nil {
		t.Fatalf("NewFallback: %v", err)
	}
	app, err := Build(Config{Guardfile: gf})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	InstallFallback(app, gf, fb)

	// Forward reads the caller's argv, which for a replacement is the tool's.
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"gh", "pr", "view", "12", "--json", "title"}

	if err := app.Run(context.Background(), os.Args); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Join(gotArgv, " ") != "pr view 12 --json title" {
		t.Errorf("argv = %v, want the caller's verbatim", gotArgv)
	}
}

func TestWithholdAlternativeMayNameAnUngrantedVerbUnderDefaultAllow(t *testing.T) {
	// Without the declaration an ungranted alternative is a dead end, which is
	// why it is refused. Under it, every unnamed verb is a live path.
	src := `wrap gh {
    exec gh
    %s
    can run status
    withhold pr create {
        reason "pull requests go through Forgejo"
        alternative "pr list"
    }
}`
	// The stub-vs-grant check runs at Build rather than Parse.
	plain := parseOrFail(t, strings.Replace(src, "%s", "", 1))
	if _, err := Build(Config{Guardfile: plain}); err == nil {
		t.Fatal("an ungranted alternative must be refused without default-allow")
	}
	open := parseOrFail(t, strings.Replace(src, "%s", `default-allow { reason "one boundary" }`, 1))
	if _, err := Build(Config{Guardfile: open}); err != nil {
		t.Fatalf("default-allow must make an ungranted alternative reachable: %v", err)
	}
}

func TestDescribeSaysTheDefaultIsInverted(t *testing.T) {
	// A reader of the model sees the grants; without this they read the rest as
	// refused, which is the opposite of what runs.
	s := Describe(parseOrFail(t, defaultAllowGuardfile))

	if !strings.Contains(s.DefaultAllow, "read tool") {
		t.Errorf("Surface.DefaultAllow = %q, want the authored reason", s.DefaultAllow)
	}
}
