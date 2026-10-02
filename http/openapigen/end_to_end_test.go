package openapigen_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/openapigen"
	"github.com/coilyco/umbra/http/specverb"
)

// TestEmitFromARealGuardfile drives the whole path a caller uses: a committed
// guardfile and spec, through specverb's own resolver, into a document.
func TestEmitFromARealGuardfile(t *testing.T) {
	gfSrc, err := os.ReadFile("../specverb/testdata/forgejo.kdl")
	if err != nil {
		t.Fatalf("read guardfile: %v", err)
	}
	spec, err := os.ReadFile("../specverb/testdata/forgejo.swagger.v1.json")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	gf, err := guardfile.Parse(gfSrc)
	if err != nil {
		t.Fatalf("parse guardfile: %v", err)
	}
	descs, rt, err := specverb.Descriptors(specverb.DescriptorConfig{Guardfile: gf, Spec: spec})
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	if len(descs) != 3 {
		t.Fatalf("descriptors = %d, want the three granted verbs", len(descs))
	}

	raw, skipped, err := openapigen.Emit(descs, openapigen.Config{
		Title: "ward-ops-forgejo", Version: "1.0.0", BaseURL: rt.BaseURL,
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v, want none: every granted verb here reaches a URL", skipped)
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Summary     string `json:"summary"`
			Destructive bool   `json:"x-umbra-destructive"`
			Grant       string `json:"x-umbra-grant"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("emitted document is not JSON: %v", err)
	}
	if doc.OpenAPI != openapigen.Version {
		t.Errorf("openapi = %q, want %q", doc.OpenAPI, openapigen.Version)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://forgejo.example/api/v1" {
		t.Errorf("servers = %v, want the guardfile base-url", doc.Servers)
	}

	// The granted subset only: this spec carries far more than three operations,
	// and a document listing any of the rest would over-state the grant.
	total := 0
	for _, item := range doc.Paths {
		total += len(item)
	}
	if total != 3 {
		t.Fatalf("emitted %d operations, want exactly the 3 granted", total)
	}

	del, ok := doc.Paths["/repos/{owner}/{repo}"]["delete"]
	if !ok {
		t.Fatal("no DELETE on the repo path")
	}
	if !del.Destructive {
		t.Error("the delete grant lost its destructive marking")
	}
	if del.Summary != "irreversible: deletes the repo and all its data" {
		t.Errorf("summary = %q, want the guardfile's describe", del.Summary)
	}
	if del.Grant == "" {
		t.Error("the authorizing grant did not survive onto the operation")
	}
}
