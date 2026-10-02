package mcpverb_test

import (
	"strings"
	"testing"

	"github.com/coilyco/umbra/http/mcpverb"
)

const upstreamFull = `
description "Search the published Tandem docs index."

mcp-upstream "ac.tandem/docs-mcp" {
    url "https://tandem.ac/mcp"
    transport streamable-http
    annotation-coverage partial annotated=7 silent=6
    auth header-token {
        header "Authorization"
        prefix "Bearer "
        value env "TANDEM_TOKEN"
    }
    can "search_docs"
    can "get_doc"
}
`

func TestParseUpstream_Full(t *testing.T) {
	up, err := mcpverb.ParseUpstream([]byte(upstreamFull))
	if err != nil {
		t.Fatalf("ParseUpstream: %v", err)
	}
	if up.Name != "ac.tandem/docs-mcp" {
		t.Errorf("Name = %q", up.Name)
	}
	if up.Description != "Search the published Tandem docs index." {
		t.Errorf("Description = %q", up.Description)
	}
	if up.URL != "https://tandem.ac/mcp" {
		t.Errorf("URL = %q", up.URL)
	}
	if up.Transport != mcpverb.UpstreamTransport {
		t.Errorf("Transport = %q", up.Transport)
	}
	if got := strings.Join(up.Tools, ","); got != "search_docs,get_doc" {
		t.Errorf("Tools = %q, want stated order", got)
	}
	if up.Auth.Scheme != "header-token" || up.Auth.Header != "Authorization" || up.Auth.Prefix != "Bearer " {
		t.Errorf("Auth = %+v", up.Auth)
	}
	if got := up.Auth.Value.String(); got != "env TANDEM_TOKEN" {
		t.Errorf("Auth.Value = %q", got)
	}
	if up.Coverage == nil || up.Coverage.Kind != "partial" || up.Coverage.Annotated != 7 || up.Coverage.Silent != 6 {
		t.Errorf("Coverage = %+v", up.Coverage)
	}
	if got := strings.Join(up.Providers(), ","); got != "env" {
		t.Errorf("Providers = %q", got)
	}
}

// A guardfile that exposes nothing still says where the upstream is, so an
// empty allowlist parses rather than failing as an incomplete file.
func TestParseUpstream_EmptyAllowlistIsAStatement(t *testing.T) {
	up, err := mcpverb.ParseUpstream([]byte(`mcp-upstream "x" { url "https://h/mcp" }`))
	if err != nil {
		t.Fatalf("ParseUpstream: %v", err)
	}
	if len(up.Tools) != 0 {
		t.Errorf("Tools = %v, want none", up.Tools)
	}
	if up.Transport != mcpverb.UpstreamTransport {
		t.Errorf("Transport = %q, want the default", up.Transport)
	}
	if up.Coverage != nil {
		t.Errorf("Coverage = %+v, want nil when unstated", up.Coverage)
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		want    mcpverb.Shape
		wantErr string
	}{
		{
			name: "command",
			src:  `wrap a b { mcp stdio { command "npx" } }`,
			want: mcpverb.ShapeCommand,
		},
		{
			name: "upstream",
			src:  `mcp-upstream "x" { url "https://h/mcp" }`,
			want: mcpverb.ShapeUpstream,
		},
		{
			// The command shape reads every wrap argument as a path segment, so
			// this file is a wrapped command named `mcp` and not an upstream.
			name: "wrap mcp upstream is a command path, not an upstream",
			src:  `wrap mcp upstream "x" { mcp stdio { command "npx" } }`,
			want: mcpverb.ShapeCommand,
		},
		{
			name:    "both",
			src:     `wrap a b { mcp stdio { command "npx" } }` + "\n" + `mcp-upstream "x" { url "https://h/mcp" }`,
			wantErr: "not both",
		},
		{
			name:    "neither",
			src:     `description "hi"`,
			wantErr: "missing top-level",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mcpverb.Classify([]byte(tc.src))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			if got != tc.want {
				t.Errorf("Shape = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseUpstream_FailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "no node",
			src:     `wrap a b { mcp stdio { command "npx" } }`,
			wantErr: "missing top-level `mcp-upstream`",
		},
		{
			name:    "both shapes",
			src:     `mcp-upstream "x" { url "https://h/mcp" }` + "\n" + `wrap a b { mcp stdio { command "npx" } }`,
			wantErr: "not both",
		},
		{
			name:    "no name",
			src:     `mcp-upstream { url "https://h/mcp" }`,
			wantErr: "wants exactly one name",
		},
		{
			name:    "empty name",
			src:     `mcp-upstream "" { url "https://h/mcp" }`,
			wantErr: "must be non-empty",
		},
		{
			name:    "properties on the node",
			src:     `mcp-upstream "x" registry=true { url "https://h/mcp" }`,
			wantErr: "takes no properties",
		},
		{
			name:    "no url",
			src:     `mcp-upstream "x" { can "search" }`,
			wantErr: "needs a `url`",
		},
		{
			name:    "relative url",
			src:     `mcp-upstream "x" { url "/mcp" }`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "non-http scheme",
			src:     `mcp-upstream "x" { url "ws://h/mcp" }`,
			wantErr: "must be an absolute http or https URL",
		},
		{
			name:    "duplicate url",
			src:     `mcp-upstream "x" { url "https://h/mcp"; url "https://i/mcp" }`,
			wantErr: "duplicate `url`",
		},
		{
			name:    "unserved transport",
			src:     `mcp-upstream "x" { url "https://h/mcp"; transport stdio }`,
			wantErr: "is not served",
		},
		{
			name:    "unknown body node",
			src:     `mcp-upstream "x" { url "https://h/mcp"; base-url "https://h" }`,
			wantErr: `unknown node "base-url"`,
		},
		{
			// The command shape's grant sentence, which this shape does not take:
			// there is no leaf to name and no guard to hang.
			name:    "can call sentence",
			src:     `mcp-upstream "x" { url "https://h/mcp"; can call "search" }`,
			wantErr: "takes one bare tool name",
		},
		{
			name:    "can with a body",
			src:     `mcp-upstream "x" { url "https://h/mcp"; can "search" { describe "no" } }`,
			wantErr: "the contract stays upstream",
		},
		{
			name:    "duplicate tool",
			src:     `mcp-upstream "x" { url "https://h/mcp"; can "search"; can "search" }`,
			wantErr: `duplicate ` + "`can \"search\"`",
		},
		{
			name:    "empty tool name",
			src:     `mcp-upstream "x" { url "https://h/mcp"; can "" }`,
			wantErr: "non-empty tool name",
		},
		{
			name:    "inherit",
			src:     `mcp-upstream "x" { url "https://h/mcp"; inherit "base.mcp.kdl" }`,
			wantErr: "`inherit` is not supported",
		},
		{
			name:    "empty description",
			src:     `description ""` + "\n" + `mcp-upstream "x" { url "https://h/mcp" }`,
			wantErr: "`description` must be a non-empty string",
		},
		{
			name:    "unknown auth scheme",
			src:     `mcp-upstream "x" { url "https://h/mcp"; auth carrier-pigeon }`,
			wantErr: "auth scheme",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mcpverb.ParseUpstream([]byte(tc.src))
			if err == nil {
				t.Fatalf("ParseUpstream succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseUpstream_AnnotationCoverage(t *testing.T) {
	cases := []struct {
		name    string
		node    string
		wantErr string
	}{
		{name: "declared", node: `annotation-coverage declared annotated=4 silent=0`},
		{name: "undeclared", node: `annotation-coverage undeclared annotated=0 silent=9`},
		{
			name:    "declared with a silent tool",
			node:    `annotation-coverage declared annotated=4 silent=1`,
			wantErr: "wants silent=0",
		},
		{
			name:    "undeclared with an annotated tool",
			node:    `annotation-coverage undeclared annotated=1 silent=9`,
			wantErr: "wants annotated=0",
		},
		{
			name:    "partial with one side zero",
			node:    `annotation-coverage partial annotated=4 silent=0`,
			wantErr: "wants both counts above zero",
		},
		{
			name:    "unknown kind",
			node:    `annotation-coverage mostly annotated=4 silent=1`,
			wantErr: "unknown `annotation-coverage` kind",
		},
		{
			name:    "missing a count",
			node:    `annotation-coverage partial annotated=4`,
			wantErr: "wants both annotated= and silent=",
		},
		{
			name:    "unknown property",
			node:    `annotation-coverage partial annotated=4 silent=1 total=5`,
			wantErr: "unknown `annotation-coverage` property",
		},
		{
			// A quoted count reads as a number and is not one, which is exactly
			// the edit a hand-written marker makes.
			name:    "string count",
			node:    `annotation-coverage partial annotated="4" silent=1`,
			wantErr: "wants a whole number",
		},
		{
			name:    "negative count",
			node:    `annotation-coverage partial annotated=4 silent=-1`,
			wantErr: "must not be negative",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `mcp-upstream "x" { url "https://h/mcp"; ` + tc.node + ` }`
			_, err := mcpverb.ParseUpstream([]byte(src))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseUpstream: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
