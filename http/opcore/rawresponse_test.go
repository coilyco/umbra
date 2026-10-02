package opcore_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/valuesource"
)

// rawLogOp wires a GET leaf whose spec declared a non-JSON success media type,
// the shape a CI job-log endpoint has.
func rawLogOp(srv *httptest.Server, raw bool, restrict ...guardfile.Restriction) opcore.Operation {
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL:   srv.URL,
		Auth:      tokenAuth("s3cret"),
		Providers: valuesource.Merge(nil),
		Client:    srv.Client(),
		Restrict:  restrict,
	})
	return opcore.Operation{
		RT: rt,
		Desc: opcore.Descriptor{
			VerbName:    "test.repo.jobs.logs",
			Group:       "action-job",
			Leaf:        "logs",
			Method:      http.MethodGet,
			Path:        "/repos/{owner}/{repo}/actions/jobs/{job}/logs",
			PathParams:  []string{"owner", "repo", "job"},
			RawResponse: raw,
		},
	}
}

func logArgs() opcore.Args {
	return opcore.Args{Path: map[string]string{"owner": "o", "repo": "r", "job": "1"}}
}

// The plaintext body opens on a timestamp, so a JSON decode fails on its first
// character. That decode used to happen before the raw branch was consulted.
const plaintextLog = "2026-08-15T15:27:02.9378852Z [command]/usr/bin/git config --local\n"

func TestExecuteReturnsDeclaredPlaintextIntact(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(plaintextLog))
	}))
	defer srv.Close()

	resp, err := rawLogOp(srv, true).Execute(context.Background(), logArgs())
	if err != nil {
		t.Fatalf("a declared-plaintext body failed the call: %v", err)
	}
	if string(resp.Raw) != plaintextLog {
		t.Errorf("Raw = %q, want %q", resp.Raw, plaintextLog)
	}
	if resp.Decoded != nil {
		t.Errorf("Decoded = %v, want nil for a non-JSON body", resp.Decoded)
	}
}

// A ZIP is the other declared shape, and it is binary rather than merely
// non-JSON, so byte equality is the only assertion worth making.
func TestExecuteReturnsDeclaredBinaryIntact(t *testing.T) {
	archive := []byte("PK\x03\x04\x00\x00binary\x00bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	resp, err := rawLogOp(srv, true).Execute(context.Background(), logArgs())
	if err != nil {
		t.Fatalf("a declared-ZIP body failed the call: %v", err)
	}
	if !bytes.Equal(resp.Raw, archive) {
		t.Errorf("Raw = %q, want %q", resp.Raw, archive)
	}
}

// The fail-safe direction is unchanged: an operation that did not declare a
// non-JSON response still refuses a body it cannot parse.
func TestExecuteStillRejectsUndeclaredNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(plaintextLog))
	}))
	defer srv.Close()

	_, err := rawLogOp(srv, false).Execute(context.Background(), logArgs())
	if err == nil {
		t.Fatal("an undeclared non-JSON body was accepted")
	}
	if kindOf(err) != "internal" {
		t.Errorf("kind = %q, want internal", kindOf(err))
	}
}

// A raw leaf must still fail closed on an upstream rejection rather than hand
// back the error page as if it were log bytes.
func TestExecuteRawStillFailsOnUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"job not found"}`))
	}))
	defer srv.Close()

	_, err := rawLogOp(srv, true).Execute(context.Background(), logArgs())
	if err == nil {
		t.Fatal("a 404 was reported as a successful raw body")
	}
	if kindOf(err) != "upstream_failed" {
		t.Errorf("kind = %q, want upstream_failed", kindOf(err))
	}
}

// An empty raw body is a real state for a job that produced no output, and it
// must not be confused with a failure.
func TestExecuteRawAcceptsAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()

	resp, err := rawLogOp(srv, true).Execute(context.Background(), logArgs())
	if err != nil {
		t.Fatalf("an empty raw body failed the call: %v", err)
	}
	if len(resp.Raw) != 0 {
		t.Errorf("Raw = %q, want empty", resp.Raw)
	}
}

// rawGrantSrc is the hand-written guardfile shape part two exists for: an Atom
// feed, which no spec media type declares because there is no spec.
const rawGrantSrc = `wrap ward mcp reddit {
    auth bearer { value env "T" }
    can list post {
        path "/r/{sub}/new.rss"
        raw-response
    }
}`

func TestParseInlineRawResponseNode(t *testing.T) {
	descs, _ := parseInline(t, rawGrantSrc)
	if d := descByLeaf(t, descs, "list"); !d.RawResponse {
		t.Error("`raw-response` did not set Descriptor.RawResponse")
	}
}

// The fail-safe direction: a grant that says nothing is parsed exactly as it
// was before this node existed.
func TestParseInlineUndeclaredGrantStaysParsed(t *testing.T) {
	descs, _ := parseInline(t, inlineSrc)
	for _, leaf := range []string{"create", "close", "delete"} {
		if descByLeaf(t, descs, leaf).RawResponse {
			t.Errorf("leaf %q was not declared raw but came back raw", leaf)
		}
	}
}

// A half-specified declaration is a parse error rather than a silent
// passthrough, matching the rest of the grammar.
func TestParseInlineRawResponseFailsClosed(t *testing.T) {
	cases := map[string]string{
		"argument":  `raw-response "yes"`,
		"property":  `raw-response enabled=#true`,
		"block":     "raw-response {\n            format \"atom\"\n        }",
		"duplicate": "raw-response\n        raw-response",
		// fail-when needs a decoded response, which a raw body never has, so
		// the pair would leave the postcondition inert.
		"with fail-when": "raw-response\n        fail-when \"a == null\"",
	}
	for name, decl := range cases {
		t.Run(name, func(t *testing.T) {
			src := `wrap ward mcp reddit {
    auth bearer { value env "T" }
    can list post {
        path "/r/{sub}/new.rss"
        ` + decl + `
    }
}`
			if _, _, err := opcore.ParseInline([]byte(src)); err == nil {
				t.Fatal("want a parse error, got nil")
			}
		})
	}
}

// End to end: the node reaches the runtime, which is the whole point of part
// two. An Atom body would fail a JSON decode on its first character.
func TestParseInlineRawGrantExecutesRaw(t *testing.T) {
	const atom = `<?xml version="1.0" encoding="UTF-8"?><feed><title>r/golang</title></feed>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		_, _ = w.Write([]byte(atom))
	}))
	defer srv.Close()

	descs, cfg, err := opcore.ParseInline([]byte(rawGrantSrc))
	if err != nil {
		t.Fatalf("ParseInline: %v", err)
	}
	cfg.BaseURL = srv.URL
	cfg.Auth = tokenAuth("s3cret")
	cfg.Providers = valuesource.Merge(nil)
	cfg.Client = srv.Client()

	op := opcore.Operation{RT: opcore.NewRuntime(cfg), Desc: descs[0]}
	resp, err := op.Execute(context.Background(), opcore.Args{Path: map[string]string{"sub": "golang"}})
	if err != nil {
		t.Fatalf("a guardfile-declared raw body failed the call: %v", err)
	}
	if string(resp.Raw) != atom {
		t.Errorf("Raw = %q, want %q", resp.Raw, atom)
	}
}

// The guardfile's restrict gate runs before the request, so a raw leaf gains no
// exemption from it.
func TestExecuteRawStillHonorsRestrict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(plaintextLog))
	}))
	defer srv.Close()

	op := rawLogOp(srv, true, guardfile.Restriction{Param: "owner", Globs: []string{"coily*"}})
	if _, err := op.Execute(context.Background(), logArgs()); err == nil {
		t.Fatal("a raw leaf bypassed restrict")
	}
}
