package specverb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
)

// loadProvingGuardfile parses a testdata Guardfile and the spec it names.
func loadProvingGuardfile(t *testing.T, name string) DescriptorConfig {
	t.Helper()
	gfBytes, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read guardfile: %v", err)
	}
	gf, err := guardfile.Parse(gfBytes)
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", gf.Spec))
	if err != nil {
		t.Fatalf("read spec %q: %v", gf.Spec, err)
	}
	return DescriptorConfig{Guardfile: gf, Spec: raw}
}

// descriptorLeafSet indexes descriptors by "group leaf" for order-free assertions.
func descriptorLeafSet(t *testing.T, cfg DescriptorConfig) map[string]string {
	t.Helper()
	descs, _, err := Descriptors(cfg)
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	got := map[string]string{}
	for _, d := range descs {
		got[d.Group+" "+d.Leaf] = d.Method + " " + d.Path
	}
	return got
}

// TestDescriptorsResolvesTheSameOperations pins each grant to the {method, path}
// the cli projection mounts, so neither source is a parallel reading of the other.
func TestDescriptorsResolvesTheSameOperations(t *testing.T) {
	got := descriptorLeafSet(t, loadProvingGuardfile(t, "forgejo.kdl"))
	want := map[string]string{
		"repo get":    "GET /repos/{owner}/{repo}",
		"repo create": "POST /user/repos",
		"repo delete": "DELETE /repos/{owner}/{repo}",
	}
	if len(got) != len(want) {
		t.Fatalf("descriptor count = %d, want %d: %v", len(got), len(want), got)
	}
	for leaf, op := range want {
		if got[leaf] != op {
			t.Errorf("%s = %q, want %q", leaf, got[leaf], op)
		}
	}
}

// TestDescriptorsCarriesGrantDetail covers the per-leaf payload a consumer
// projects into a description and a destructive annotation.
func TestDescriptorsCarriesGrantDetail(t *testing.T) {
	descs, _, err := Descriptors(loadProvingGuardfile(t, "forgejo.kdl"))
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	for _, d := range descs {
		if d.Leaf != "delete" {
			continue
		}
		if !d.Destructive {
			t.Error("repo delete is not marked Destructive")
		}
		if !strings.Contains(d.Describe, "irreversible") {
			t.Errorf("repo delete Describe = %q, want the guardfile note", d.Describe)
		}
		if d.VerbName != "ward.ops.forgejo.repo.delete" {
			t.Errorf("VerbName = %q, want the dotted audit name", d.VerbName)
		}
		return
	}
	t.Fatal("no repo delete descriptor")
}

// TestDescriptorsRuntimeConfigMirrorsTheGuardfile pins what Execute needs to gate
// a call. An empty config here would fire unauthenticated and ungated.
func TestDescriptorsRuntimeConfigMirrorsTheGuardfile(t *testing.T) {
	cfg := loadProvingGuardfile(t, "forgejo.kdl")
	_, rt, err := Descriptors(cfg)
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	if rt.BaseURL != "https://forgejo.example/api/v1" {
		t.Errorf("BaseURL = %q", rt.BaseURL)
	}
	if rt.Auth.Header != "Authorization" {
		t.Errorf("Auth.Header = %q, want the guardfile header", rt.Auth.Header)
	}
	if rt.Providers == nil {
		t.Error("Providers is nil; the built-in resolvers did not merge")
	}
}

// TestDescriptorsBaseURLOverride covers the one field a consumer supplies that
// the guardfile does not.
func TestDescriptorsBaseURLOverride(t *testing.T) {
	cfg := loadProvingGuardfile(t, "forgejo.kdl")
	cfg.BaseURL = "https://forgejo.example.test/api/v1"
	_, rt, err := Descriptors(cfg)
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	if rt.BaseURL != cfg.BaseURL {
		t.Errorf("BaseURL = %q, want the override", rt.BaseURL)
	}
}

// TestDescriptorsDenyIsAbsence is the load-bearing one: the cli mounts a denied
// leaf as a refusing command, a descriptor consumer must receive nothing.
func TestDescriptorsDenyIsAbsence(t *testing.T) {
	got := descriptorLeafSet(t, loadProvingGuardfile(t, "forgejo-readonly.kdl"))
	if len(got) == 0 {
		t.Fatal("wildcard read grants mounted nothing")
	}
	for leaf := range got {
		if strings.HasSuffix(leaf, " delete") {
			t.Errorf("%q is present; `never delete \"*\"` must resolve to absence, not a leaf", leaf)
		}
	}
}

// TestDescriptorsRefusesCLIOnlyNodes covers the fail-closed contract: a dropped
// `action` would serve the generated leaf its author replaced.
func TestDescriptorsRefusesCLIOnlyNodes(t *testing.T) {
	base := loadProvingGuardfile(t, "forgejo.kdl")

	t.Run("action", func(t *testing.T) {
		cfg := base
		gf := *cfg.Guardfile
		gf.Actions = []guardfile.Action{{}}
		cfg.Guardfile = &gf
		if _, _, err := Descriptors(cfg); err == nil {
			t.Fatal("an `action` node was accepted")
		} else if !strings.Contains(err.Error(), "action") {
			t.Errorf("error does not name the node: %v", err)
		}
	})

	t.Run("fetch", func(t *testing.T) {
		cfg := base
		gf := *cfg.Guardfile
		gf.Fetches = []guardfile.Fetch{{}}
		cfg.Guardfile = &gf
		if _, _, err := Descriptors(cfg); err == nil {
			t.Fatal("a `fetch` node was accepted")
		} else if !strings.Contains(err.Error(), "fetch") {
			t.Errorf("error does not name the node: %v", err)
		}
	})
}

// TestDescriptorsFailsClosed covers the empty inputs, so a consumer never
// receives a usable-looking zero surface.
func TestDescriptorsFailsClosed(t *testing.T) {
	if _, _, err := Descriptors(DescriptorConfig{}); err == nil {
		t.Error("a nil Guardfile was accepted")
	}
	cfg := loadProvingGuardfile(t, "forgejo.kdl")
	gf := *cfg.Guardfile
	gf.Grants = nil
	cfg.Guardfile = &gf
	if _, _, err := Descriptors(cfg); err == nil {
		t.Error("a Guardfile granting nothing was accepted")
	}
}

// TestDescriptorsMatchTheCLISurface pins the two sources against each other on a
// guardfile that denies nothing, so neither projection can drift unnoticed.
func TestDescriptorsMatchTheCLISurface(t *testing.T) {
	cfg := loadProvingGuardfile(t, "forgejo.kdl")
	mounted := leafSet(t, cfg.Guardfile, cfg.Spec)

	descs, _, err := Descriptors(cfg)
	if err != nil {
		t.Fatalf("Descriptors: %v", err)
	}
	resolved := map[string]bool{}
	for _, d := range descs {
		resolved[d.Group+"/"+d.Leaf] = true
	}
	for leaf := range mounted {
		if !resolved[leaf] {
			t.Errorf("cli mounts %q with no descriptor", leaf)
		}
	}
	for leaf := range resolved {
		if _, ok := mounted[leaf]; !ok {
			t.Errorf("descriptor %q mounts on no cli leaf", leaf)
		}
	}
}
