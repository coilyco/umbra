package guardfile_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// overrideHeader is the wrap prelude every standalone override test reuses.
const overrideHeader = `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
`

// TestOverrideParses proves `override can <verb> <resource>` lowers to a `can`
// grant flagged Override, the typed bridge the engine reads to cross a deny.
func TestOverrideParses(t *testing.T) {
	gf, err := guardfile.Parse([]byte(overrideHeader + `
    never delete "*"
    override can delete repo
}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, g := range gf.Grants {
		if g.Override {
			found = true
			if g.Modal != "can" || g.Verb != "delete" || g.Resource != "repo" {
				t.Errorf("override grant misparsed: %+v", g)
			}
		}
	}
	if !found {
		t.Fatal("no Override grant parsed")
	}
}

// TestOverrideRequiresCan proves the bare `override <verb> <resource>` form (no
// `can`) is a fail-closed error: an override only ever re-grants.
func TestOverrideRequiresCan(t *testing.T) {
	_, err := guardfile.Parse([]byte(overrideHeader + `
    never delete "*"
    override delete repo
}`))
	if err == nil || !strings.Contains(err.Error(), "override can") {
		t.Fatalf("bare `override delete repo` should fail pointing at `override can`: %v", err)
	}
}

// TestOverrideRejectsWildcard proves `override can <verb> "*"` is refused: an
// override names one resource so every escalation is enumerated by name.
func TestOverrideRejectsWildcard(t *testing.T) {
	_, err := guardfile.Parse([]byte(overrideHeader + `
    never delete "*"
    override can delete "*"
}`))
	if err == nil || !strings.Contains(err.Error(), "names one resource") {
		t.Fatalf("wildcard override should be rejected: %v", err)
	}
}

// TestOverrideNoOpRejected proves an override that crosses no deny is a build
// error: silently it would be a plain `can`, the fail-open trap it exists to stop.
func TestOverrideNoOpRejected(t *testing.T) {
	_, err := guardfile.Parse([]byte(overrideHeader + `
    can get "*"
    override can delete repo
}`))
	if err == nil || !strings.Contains(err.Error(), "lifts no") {
		t.Fatalf("a no-op override (no matching deny) should fail closed: %v", err)
	}
}

// TestOverrideCrossesVerbStarDeny proves a `never <verb> "*"` satisfies an override
// of any one resource under that verb (the verb-global deny covers it).
func TestOverrideCrossesVerbStarDeny(t *testing.T) {
	if _, err := guardfile.Parse([]byte(overrideHeader + `
    never delete "*"
    override can delete repo
}`)); err != nil {
		t.Fatalf("a verb-global `never delete \"*\"` should cover `override can delete repo`: %v", err)
	}
}

// TestInheritRestrictPropagates proves a base `restrict` now inherits to an
// un-restating higher tier (the earlier child-local behavior superseded).
func TestInheritRestrictPropagates(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    restrict owner matches "coilyco-*"
    can get "*"
}
`)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can edit "*"
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(gf.Restrict) != 1 || gf.Restrict[0].Param != "owner" {
		t.Fatalf("base restrict should inherit; got %+v", gf.Restrict)
	}
}

// TestInheritRestrictChildWins proves a child restating the same restrict param
// shadows the inherited one (closer layer wins, no duplicate).
func TestInheritRestrictChildWins(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    restrict owner matches "coilyco-*"
    can get "*"
}
`)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    restrict owner matches "example-*"
    can edit "*"
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(gf.Restrict) != 1 {
		t.Fatalf("same-param restrict should dedup to one, got %+v", gf.Restrict)
	}
	if gf.Restrict[0].Globs[0] != "example-*" {
		t.Errorf("child restrict should win: %+v", gf.Restrict[0])
	}
}

// TestInheritNeverPropagates proves a base `never` inherits to an un-restating
// higher tier (deny-low, the fail-closed base).
func TestInheritNeverPropagates(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
    never delete "*"
}
`)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can edit "*"
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !grantSet(gf)["never delete *"] {
		t.Errorf("base `never delete \"*\"` should inherit; have %v", grantSet(gf))
	}
}

// TestInheritExplicitCanCrossingNeverErrors proves an explicit plain `can delete
// repo` shadowed by an inherited `never delete "*"` is a build error, not a drop.
func TestInheritExplicitCanCrossingNeverErrors(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
    never delete "*"
}
`)
	child := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can delete repo
}
`)
	_, err := guardfile.ParseFile(child)
	if err == nil {
		t.Fatal("an explicit plain `can` crossing an inherited `never` must fail closed")
	}
	if !strings.Contains(err.Error(), "override can delete repo") {
		t.Errorf("error should teach the override remedy: %v", err)
	}
}

// TestInheritWildcardCanIsExemptFromCrossing proves a wildcard `can delete "*"`
// is not a build error against an inherited `never` (its carve-outs deny silently).
func TestInheritWildcardCanIsExemptFromCrossing(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    never delete repo
}
`)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can delete "*"
}
`)
	if _, err := guardfile.ParseFile(child); err != nil {
		t.Fatalf("a wildcard `can` must not error against an inherited `never`: %v", err)
	}
}

// TestInheritOverrideLiftsInheritedNever proves a child `override can delete repo`
// over an inherited `never delete "*"` parses clean and carries the Override flag.
func TestInheritOverrideLiftsInheritedNever(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
    never delete "*"
}
`)
	child := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    override can delete repo
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	var found bool
	for _, g := range gf.Grants {
		if g.Override && g.Verb == "delete" && g.Resource == "repo" {
			found = true
		}
	}
	if !found {
		t.Errorf("override grant lost through inheritance; have %v", grantSet(gf))
	}
}

// TestInheritOverrideWithoutInheritedNeverErrors proves an override that lifts no
// inherited `never` is rejected at build time (no silent no-op escalation).
func TestInheritOverrideWithoutInheritedNeverErrors(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
}
`)
	child := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    override can delete repo
}
`)
	_, err := guardfile.ParseFile(child)
	if err == nil {
		t.Fatal("an override lifting no inherited `never` must fail closed")
	}
	if !strings.Contains(err.Error(), "lifts no inherited") {
		t.Errorf("error should explain there is no inherited never to lift: %v", err)
	}
}

// TestInheritOverrideWhenChildAlsoRestatesNever proves a tier that both restates a
// base `never` and overrides one resource is accepted (dedup must not hide it).
func TestInheritOverrideWhenChildAlsoRestatesNever(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
    never delete "*"
}
`)
	child := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    never delete "*"
    override can delete repo
}
`)
	if _, err := guardfile.ParseFile(child); err != nil {
		t.Fatalf("override over a restated-and-inherited never should be valid: %v", err)
	}
}

// TestInheritOverrideFlattenStaysParseable is a guard that the flatten round-trip
// preserves an override node verbatim through emit→reparse.
func TestInheritOverrideFlattenStaysParseable(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    never delete "*"
}
`)
	child := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can get "*"
    override can delete repo
}
`)
	flat, err := guardfile.Flatten(child)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if !strings.Contains(string(flat), "override") {
		t.Errorf("flattened output dropped the override directive:\n%s", flat)
	}
	if _, err := guardfile.Parse(flat); err != nil {
		t.Errorf("flattened output must reparse: %v", err)
	}
	_ = filepath.Dir(child)
}
