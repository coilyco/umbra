package specverb

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
)

// TestInlineAndResolvedDescriptorsAreEqual is the shared golden: an op stated
// inline and the same op resolved against the spec produce equal Descriptors.
func TestInlineAndResolvedDescriptorsAreEqual(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "forgejo.swagger.v1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	spec, err := parseSwagger(raw)
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	group := []string{"ward", "ops", "forgejo"}

	// Two path-only ops whose resolved shape carries no spec-derived types: a plain
	// GET and a destructive DELETE, both at /repos/{owner}/{repo}.
	cases := []struct {
		verb, resource, inlinePath string
	}{
		{"get", "repo", "/repos/{owner}/{repo}"},
		{"delete", "repo", "/repos/{owner}/{repo}"},
	}
	for _, c := range cases {
		resolved, rerr := resolveDescriptor(spec, group, guardfile.Grant{Modal: "can", Verb: c.verb, Resource: c.resource})
		if rerr != nil {
			t.Fatalf("resolve %s %s: %v", c.verb, c.resource, rerr)
		}
		descs, _, ierr := opcore.ParseInline([]byte(
			"wrap ward ops forgejo {\n" +
				"  auth header-token { header \"Authorization\"; prefix \"token \"; value ssm \"/forgejo/api-token\" }\n" +
				"  can " + c.verb + " " + c.resource + " { path \"" + c.inlinePath + "\" }\n" +
				"}"))
		if ierr != nil {
			t.Fatalf("inline %s %s: %v", c.verb, c.resource, ierr)
		}
		if len(descs) != 1 {
			t.Fatalf("inline %s %s: got %d descriptors, want 1", c.verb, c.resource, len(descs))
		}
		if !reflect.DeepEqual(descs[0], resolved) {
			t.Errorf("%s %s descriptors differ:\n inline   = %+v\n resolved = %+v", c.verb, c.resource, descs[0], resolved)
		}
	}
}
