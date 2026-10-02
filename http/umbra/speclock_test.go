package umbra

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/umbra/codegen"
)

func TestSpecLockEncodingIsDeterministicAndRoundTrips(t *testing.T) {
	want := []byte(`{"swagger":"2.0","paths":{}}`)
	first, err := encodeSpecLock(want)
	if err != nil {
		t.Fatalf("encode first lock: %v", err)
	}
	second, err := encodeSpecLock(want)
	if err != nil {
		t.Fatalf("encode second lock: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical spec locks encoded to different bytes")
	}
	if len(first) < 2 || first[0] != 0x1f || first[1] != 0x8b {
		t.Fatalf("encoded lock does not carry gzip magic: %x", first)
	}
	got, err := decodeSpecLock(first)
	if err != nil {
		t.Fatalf("decode lock: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded lock = %q, want %q", got, want)
	}
}

func TestDecodeSpecLockRejectsInvalidGzip(t *testing.T) {
	if _, err := decodeSpecLock([]byte("plain json")); err == nil {
		t.Fatal("invalid gzip lock decoded without error")
	}
}

func TestWriteSpecLockEncodesAndRemovesLegacySibling(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "api.lock.json")
	encodedPath := legacyPath + ".gz"
	want := []byte(`{"source":"generated"}`)
	if err := os.WriteFile(legacyPath, []byte(`{"source":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSpecLock(encodedPath, want); err != nil {
		t.Fatalf("write encoded lock: %v", err)
	}
	raw, err := os.ReadFile(encodedPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSpecLock(raw)
	if err != nil {
		t.Fatalf("decode written lock: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written lock = %q, want %q", got, want)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy lock remains after migration: %v", err)
	}
}

func TestReadSpecLockPrefersEncodedAndFallsBackToLegacy(t *testing.T) {
	dir := t.TempDir()
	m := member{Params: codegen.Params{SpecLockName: "api.lock.json.gz"}}
	encodedWant := []byte(`{"source":"encoded"}`)
	encoded, err := encodeSpecLock(encodedWant)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api.lock.json.gz"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api.lock.json"), []byte(`{"source":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSpecLock(dir, m)
	if err != nil {
		t.Fatalf("read encoded lock: %v", err)
	}
	if !bytes.Equal(got, encodedWant) {
		t.Fatalf("encoded lock = %q, want %q", got, encodedWant)
	}

	if err := os.Remove(filepath.Join(dir, "api.lock.json.gz")); err != nil {
		t.Fatal(err)
	}
	got, err = readSpecLock(dir, m)
	if err != nil {
		t.Fatalf("read legacy lock: %v", err)
	}
	if !strings.Contains(string(got), "legacy") {
		t.Fatalf("legacy fallback = %q", got)
	}
}
