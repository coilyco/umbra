package umbra

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/umbra/codegen"
)

func TestReadSpecSourceDecodesGzipAndKeepsPlainCompatible(t *testing.T) {
	dir := t.TempDir()
	want := []byte(`{"swagger":"2.0","paths":{}}`)
	encoded, err := encodeSpecLock(want)
	if err != nil {
		t.Fatal(err)
	}
	encodedPath := filepath.Join(dir, "api.json.gz")
	if err := os.WriteFile(encodedPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSpecSource(encodedPath)
	if err != nil {
		t.Fatalf("read gzip source: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded source = %q, want %q", got, want)
	}

	plainPath := filepath.Join(dir, "api.json")
	if err := os.WriteFile(plainPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = readSpecSource(plainPath)
	if err != nil {
		t.Fatalf("read plain source: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("plain source = %q, want %q", got, want)
	}
}

func TestDecodeGzipSpecRejectsOversizedSource(t *testing.T) {
	encoded, err := encodeSpecLock([]byte("12345"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeGzipSpec(encoded, "vendored spec", 4); err == nil {
		t.Fatal("oversized gzip source decoded without error")
	}
}

func TestLockSpecsAcceptsGzipVendoredSource(t *testing.T) {
	dir := t.TempDir()
	spec := []byte(`{
		"swagger": "2.0",
		"info": {"title": "test", "version": "1"},
		"paths": {
			"/repos/{owner}/{repo}": {
				"get": {
					"operationId": "repoGet",
					"responses": {"200": {"description": "ok"}}
				}
			}
		}
	}`)
	encoded, err := encodeSpecLock(spec)
	if err != nil {
		t.Fatal(err)
	}
	sourceName := "forgejo.swagger.v1.json.gz"
	if err := os.WriteFile(filepath.Join(dir, sourceName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	guardfilePath := filepath.Join(dir, "forgejo.kdl")
	guardfileSource := `wrap test ops forgejo {
		spec forgejo.swagger.v1.json.gz
		base-url "example.invalid/api/v1"
		auth header-token { header Authorization; prefix "token "; value env TEST_TOKEN }
		can read repos { op repoGet }
	}`
	if err := os.WriteFile(guardfilePath, []byte(guardfileSource), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readMember(guardfilePath, "forgejo.kdl")
	if err != nil {
		t.Fatal(err)
	}
	specs, err := lockSpecs(&group{Dir: dir, Binary: "test", Members: []member{m}})
	if err != nil {
		t.Fatalf("lock gzip source: %v", err)
	}
	logical := specs["forgejo.kdl"]
	if len(logical) == 0 {
		t.Fatal("lock operation returned no logical spec")
	}
	lockPath := filepath.Join(dir, "forgejo.swagger.lock.json.gz")
	lockBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeSpecLock(lockBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, logical) {
		t.Fatal("encoded lock differs from the pruned gzip source")
	}
}

func TestLoadFullSpecRejectsInvalidVendoredGzip(t *testing.T) {
	dir := t.TempDir()
	sourceName := "api.json.gz"
	if err := os.WriteFile(filepath.Join(dir, sourceName), []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := member{
		SourcePath: filepath.Join(dir, "api.kdl"),
		GF:         &guardfile.Guardfile{Spec: sourceName},
		Params:     codegen.Params{SpecURL: "https://example.invalid/swagger.v1.json"},
	}
	_, err := loadFullSpec(m)
	if err == nil {
		t.Fatal("invalid vendored gzip fell back to the network")
	}
	if !strings.Contains(err.Error(), "decode vendored spec") {
		t.Fatalf("invalid gzip error = %v", err)
	}
}
