package specverb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coilyco/umbra/cli/verb"
	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
	"github.com/coilyco/umbra/pkg/valuesource"
	"github.com/urfave/cli/v3"
)

func TestQueryAliasReferenceUsesLocalAndUpstreamNames(t *testing.T) {
	params := paramsOf(opDescriptor{QueryFlags: []fieldFlag{
		{Name: "search_query", UpstreamName: "query", Type: "string"},
	}})
	if len(params) != 1 {
		t.Fatalf("params = %+v, want one query input", params)
	}
	if params[0].Name != "search_query" || params[0].UpstreamName != "query" {
		t.Errorf("aliased param = %+v", params[0])
	}
	for _, rendered := range []string{
		strings.Join(optionLines(params), "\n"),
		strings.Join(paramHelpLines(params), "\n"),
	} {
		if !strings.Contains(rendered, "search_query") || !strings.Contains(rendered, "query") {
			t.Errorf("reference omitted local or upstream name: %q", rendered)
		}
	}
}

func TestAssembleQueryUsesUpstreamName(t *testing.T) {
	f := opcore.Field{Name: "search_query", UpstreamName: "query"}
	got := ""
	cmd := &cli.Command{
		Flags: fieldFlagsToCLI([]fieldFlag{f}),
		Action: func(_ context.Context, c *cli.Command) error {
			var err error
			got, err = resolveCLIQuery(c, opDescriptor{QueryFlags: []fieldFlag{f}})
			return err
		},
	}
	if err := cmd.Run(context.Background(), []string{"test", "--search_query", "cards"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "?query=cards" {
		t.Errorf("query = %q, want ?query=cards", got)
	}
}

func TestAssembleQueryRepeatsArrayValuesInInputOrder(t *testing.T) {
	f := opcore.Field{Name: "author_id", Type: "array", Items: "string"}
	got := ""
	cmd := &cli.Command{
		Flags: fieldFlagsToCLI([]fieldFlag{f}),
		Action: func(_ context.Context, c *cli.Command) error {
			var err error
			got, err = resolveCLIQuery(c, opDescriptor{QueryFlags: []fieldFlag{f}})
			return err
		},
	}
	if err := cmd.Run(context.Background(), []string{
		"test",
		"--author_id", "second",
		"--author_id", "first",
		"--author_id", "third",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got != "?author_id=second&author_id=first&author_id=third" {
		t.Errorf("query = %q, want repeated keys in input order", got)
	}
}

func TestBuildLeafRejectsInvalidTypedQueryBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	rt := typedQueryRuntime(srv)
	cases := map[string][]string{
		"integer below minimum": {"--limit", "0"},
		"missing required":      {},
		"array above max-items": {
			"--limit", "1",
			"--author_id", "a",
			"--author_id", "b",
			"--author_id", "c",
		},
		"mutually exclusive": {
			"--limit", "1",
			"--before", "10",
			"--after", "20",
		},
		"invalid boolean array item": {
			"--limit", "1",
			"--enabled", "true",
			"--enabled", "not-bool",
		},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			before := calls.Load()
			leaf := rt.buildLeaf(typedQueryDescriptor())
			err := leaf.Run(context.Background(), append([]string{"list"}, args...))
			if err == nil {
				t.Fatal("invalid typed query should fail")
			}
			if got := calls.Load(); got != before {
				t.Fatalf("upstream calls = %d, want %d", got, before)
			}
		})
	}
}

func TestBuildLeafTypedQueryPreservesRepeatedOrder(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	leaf := typedQueryRuntime(srv).buildLeaf(typedQueryDescriptor())
	err := leaf.Run(context.Background(), []string{
		"list",
		"--limit", "10",
		"--author_id", "second",
		"--author_id", "first",
		"--enabled", "true",
		"--enabled", "false",
	})
	if err != nil {
		t.Fatalf("run valid typed query: %v", err)
	}
	want := "author_id=second&author_id=first&enabled=true&enabled=false&limit=10"
	if gotQuery != want {
		t.Errorf("query = %q, want %q", gotQuery, want)
	}
}

func typedQueryDescriptor() opDescriptor {
	minimum := float64(1)
	maximum := float64(100)
	maxItems := 2
	return opDescriptor{
		VerbName: "test.message.list",
		Leaf:     "list",
		Method:   http.MethodGet,
		Path:     "/messages",
		QueryFlags: []fieldFlag{
			{Name: "limit", Type: "integer", Required: true, Minimum: &minimum, Maximum: &maximum},
			{Name: "author_id", Type: "array", Items: "string", MaxItems: &maxItems},
			{Name: "enabled", Type: "array", Items: "boolean"},
			{Name: "before", Type: "string"},
			{Name: "after", Type: "string"},
		},
		QueryExclusive: [][]string{{"before", "after"}},
	}
}

func typedQueryRuntime(srv *httptest.Server) *runtime {
	return &runtime{
		Runtime: opcore.NewRuntime(opcore.RuntimeConfig{
			BaseURL: srv.URL,
			Auth: guardfile.Auth{
				Scheme: "bearer",
				Header: "Authorization",
				Prefix: "Bearer ",
				Value:  guardfile.ValueChain{{Provider: "literal", Address: "test-token"}},
			},
			Providers: valuesource.Merge(nil),
			Client:    srv.Client(),
		}),
		wrap: func(spec verb.Spec) cli.ActionFunc { return spec.Action },
	}
}
