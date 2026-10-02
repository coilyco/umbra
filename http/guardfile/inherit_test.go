package guardfile_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// writeGuardfile drops src at dir/name and returns the path.
func writeGuardfile(t *testing.T, dir, name, src string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// grantSet keys the parsed grants by "modal verb resource" for membership asserts.
func grantSet(gf *guardfile.Guardfile) map[string]bool {
	out := map[string]bool{}
	for _, g := range gf.Grants {
		out[g.Modal+" "+g.Verb+" "+g.Resource] = true
	}
	return out
}

const readTier = `wrap ward-kdl ops forgejo {
    spec forgejo.swagger.v1.json
    base-url "https://forgejo.example/api/v1"
    auth header-token {
        header Authorization
        prefix "token "
        value ssm "/forgejo/api-token"
    }

    can get "*"
    can list "*"
}
`

func TestInheritLayeredChain(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", readTier)
	writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can create "*"
    can edit "*"
}
`)
	admin := writeGuardfile(t, dir, "admin.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "write.guardfile.kdl"
    can delete "*"
}
`)

	gf, err := guardfile.ParseFile(admin)
	if err != nil {
		t.Fatalf("ParseFile admin: %v", err)
	}
	got := grantSet(gf)
	for _, want := range []string{
		"can get *", "can list *", // from read, two tiers up
		"can create *", "can edit *", // from write, one tier up
		"can delete *", // child's own
	} {
		if !got[want] {
			t.Errorf("merged grants missing %q; have %v", want, got)
		}
	}
	// Singletons flow down the whole chain when the child declares none.
	if gf.Spec != "forgejo.swagger.v1.json" {
		t.Errorf("inherited spec = %q, want forgejo.swagger.v1.json", gf.Spec)
	}
	if gf.BaseURL != "https://forgejo.example/api/v1" {
		t.Errorf("inherited base-url = %q", gf.BaseURL)
	}
	if gf.Auth.Scheme != "header-token" || gf.Auth.Value.String() != "ssm /forgejo/api-token" {
		t.Errorf("inherited auth not carried: %+v", gf.Auth)
	}
	// The emit→reparse round-trip must preserve significant trailing whitespace.
	if gf.Auth.Prefix != "token " {
		t.Errorf("auth prefix lost significant whitespace through inheritance: %q", gf.Auth.Prefix)
	}
}

func TestInheritWildcardComposes(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", readTier)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can edit "*"
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	// The parent's wildcard grants survive the merge as wildcard grants, so the
	// wildcard expansion runs over them unchanged.
	var wildcards int
	for _, g := range gf.Grants {
		if g.Resource == "*" && !g.Wildcard {
			t.Errorf("grant %q lost its Wildcard flag through inheritance", g.Verb)
		}
		if g.Wildcard {
			wildcards++
		}
	}
	if wildcards != 3 { // get + list (inherited) + edit (child)
		t.Errorf("wildcard grants after merge = %d, want 3", wildcards)
	}
}

func TestInheritSingletonOverride(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", readTier)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    base-url "https://override.example/api/v1"
    can edit "*"
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if gf.BaseURL != "https://override.example/api/v1" {
		t.Errorf("child base-url should win: got %q", gf.BaseURL)
	}
	// spec/auth still inherited (child declared none of its own).
	if gf.Spec != "forgejo.swagger.v1.json" || gf.Auth.Scheme != "header-token" {
		t.Errorf("non-overridden singletons not inherited: spec=%q auth=%q", gf.Spec, gf.Auth.Scheme)
	}
}

func TestInheritDedupKeepsChildGrant(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "read.guardfile.kdl", readTier)
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can get "*" {
        describe "child override of the inherited read grant"
    }
}
`)
	gf, err := guardfile.ParseFile(child)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	var getGrants []guardfile.Grant
	for _, g := range gf.Grants {
		if g.Verb == "get" && g.Resource == "*" {
			getGrants = append(getGrants, g)
		}
	}
	if len(getGrants) != 1 {
		t.Fatalf("expected one deduped `can get \"*\"`, got %d", len(getGrants))
	}
	if getGrants[0].Describe != "child override of the inherited read grant" {
		t.Errorf("dedup kept the wrong grant body: %q", getGrants[0].Describe)
	}
}

func TestInheritMissingRef(t *testing.T) {
	dir := t.TempDir()
	child := writeGuardfile(t, dir, "write.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "does-not-exist.guardfile.kdl"
    can edit "*"
}
`)
	_, err := guardfile.ParseFile(child)
	if err == nil {
		t.Fatal("expected an error for a missing inherit ref")
	}
	if !strings.Contains(err.Error(), "inherit") {
		t.Errorf("error should name the inherit failure: %v", err)
	}
}

func TestInheritCycle(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "a.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "b.guardfile.kdl"
    can edit "*"
}
`)
	writeGuardfile(t, dir, "b.guardfile.kdl", `wrap ward-kdl ops forgejo {
    inherit "a.guardfile.kdl"
    can delete "*"
}
`)
	_, err := guardfile.ParseFile(filepath.Join(dir, "a.guardfile.kdl"))
	if err == nil {
		t.Fatal("expected a cycle error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error should report a cycle: %v", err)
	}
}

func TestFlattenNoInheritIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	p := writeGuardfile(t, dir, "read.guardfile.kdl", readTier)
	out, err := guardfile.Flatten(p)
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}
	if string(out) != readTier {
		t.Errorf("a guardfile without inherit must flatten to itself, byte-for-byte")
	}
}

func TestParseRejectsRawInherit(t *testing.T) {
	// Parse (no file context) cannot resolve inherit; it must fail closed rather
	// than silently dropping the directive.
	_, err := guardfile.Parse([]byte(`wrap ward-kdl ops forgejo {
    inherit "read.guardfile.kdl"
    can edit "*"
}
`))
	if err == nil {
		t.Fatal("Parse should reject an unresolved inherit directive")
	}
	if !strings.Contains(err.Error(), "ParseFile") {
		t.Errorf("error should point at ParseFile: %v", err)
	}
}
