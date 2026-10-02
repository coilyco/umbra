package specverb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// TestInheritWildcardComposesThroughEngine proves a write tier that inherits a
// read tier of wildcard grants builds the union surface.
func TestInheritWildcardComposesThroughEngine(t *testing.T) {
	_, spec := loadFixtures(t)
	dir := t.TempDir()
	must := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	must("read.guardfile.kdl", `wrap ward ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; prefix "token "; value ssm "/forgejo/api-token" }
    can get "*"
    can list "*"
}
`)
	must("write.guardfile.kdl", `wrap ward ops forgejo {
    inherit "read.guardfile.kdl"
    can create "*"
    can edit "*"
}
`)

	gf, err := guardfile.ParseFile(filepath.Join(dir, "write.guardfile.kdl"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	leaves := leafSet(t, gf, spec)

	// Read leaves (inherited) and write leaves (child) both mount.
	for _, want := range []string{"repo/get", "issue/list", "repo/create", "issue/edit"} {
		if _, ok := leaves[want]; !ok {
			t.Errorf("composed surface missing %q; got %v", want, keysOf(leaves))
		}
	}
	// Delete was never granted at any tier: still denied by default.
	for name := range leaves {
		if strings.HasSuffix(name, "/delete") {
			t.Errorf("inherit chain leaked a delete leaf %q (no tier granted it)", name)
		}
	}
}
