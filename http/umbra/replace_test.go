package umbra

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/umbra/codegen"
)

func replacementMember(name string) member {
	return member{
		Path:   name + ".guardfile.kdl",
		Params: codegen.Params{Transport: codegen.TransportExec, Binary: "aosguard", Replace: true, Occluded: name},
	}
}

func TestNewGroupTakesItsNameFromTheToolItOccludes(t *testing.T) {
	g, err := newGroup(".", "aosguard", []member{replacementMember("git")}, "")
	if err != nil {
		t.Fatalf("newGroup: %v", err)
	}
	// The wrap group says aosguard; the binary on disk has to say git, or it
	// occludes nothing.
	if got := g.runtimeBinary(); got != "git" {
		t.Errorf("runtime binary = %q, want the occluded tool %q", got, "git")
	}
}

func TestNewGroupRefusesABinaryNameThatWouldUnoccludeIt(t *testing.T) {
	_, err := newGroup(".", "aosguard", []member{replacementMember("git")}, "guarded-git")
	if err == nil {
		t.Fatal("--binary disagreeing with `replace` must fail closed")
	}
	if !strings.Contains(err.Error(), "guarded-git") {
		t.Errorf("the refusal should name the rejected binary: %v", err)
	}
}

func TestOccludedNameRefusesAMergedReplacement(t *testing.T) {
	other := member{Path: "gh.guardfile.kdl", Params: codegen.Params{Transport: codegen.TransportExec, Binary: "aosguard"}}
	if _, err := occludedName([]member{replacementMember("git"), other}); err == nil {
		t.Fatal("a replacement merged with a second member must fail closed")
	}
}

func TestOccludedNameIsEmptyForAnOrdinaryBinary(t *testing.T) {
	other := member{Path: "gh.guardfile.kdl", Params: codegen.Params{Transport: codegen.TransportExec, Binary: "aosguard"}}
	name, err := occludedName([]member{other})
	if err != nil {
		t.Fatalf("occludedName: %v", err)
	}
	if name != "" {
		t.Errorf("occluded name = %q, want none", name)
	}
}

func TestReplacementVerbsNeedAShimDir(t *testing.T) {
	if err := Install(Options{}); err == nil {
		t.Error("install without --shim-dir must fail closed")
	}
	if _, err := Doctor(Options{}); err == nil {
		t.Error("doctor without --shim-dir must fail closed")
	}
}
