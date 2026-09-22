package opcore_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"reflect"
	"strings"
	"testing"

	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/guardfile"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/http/opcore"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/policy"
	"forgejo.coilysiren.me/coilyco-flight-deck/umbra/pkg/valuesource"
)

// kindOf extracts the coded error's stable token, or "" for a nil/uncoded error.
func kindOf(err error) string {
	var c interface{ Kind() string }
	if errors.As(err, &c) {
		return c.Kind()
	}
	return ""
}

// tokenAuth is the header auth every fixture uses: `Authorization: token <lit>`.
func tokenAuth(secret string) guardfile.Auth {
	return guardfile.Auth{
		Scheme: "header-token",
		Header: "Authorization",
		Prefix: "token ",
		Value:  guardfile.ValueChain{{Provider: "literal", Address: secret}},
	}
}

// newTestOp wires a Runtime at srv against a create-shaped leaf: POST
// /repos/{owner}/{repo}/issues with a query flag and two body flags (one required).
func newTestOp(srv *httptest.Server, auth guardfile.Auth, restrict []guardfile.Restriction) opcore.Operation {
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL:   srv.URL,
		Auth:      auth,
		Providers: valuesource.Merge(nil),
		Client:    srv.Client(),
		Restrict:  restrict,
	})
	return opcore.Operation{
		RT: rt,
		Desc: opcore.Descriptor{
			VerbName:   "test.repo.issues.create",
			Group:      "issues",
			Leaf:       "create",
			Method:     http.MethodPost,
			Path:       "/repos/{owner}/{repo}/issues",
			PathParams: []string{"owner", "repo"},
			QueryFlags: []opcore.Field{{Name: "state", Type: "string"}},
			BodyFlags: []opcore.Field{
				{Name: "title", Type: "string", Required: true},
				{Name: "count", Type: "integer"},
			},
		},
	}
}

func TestExecuteHappyPath(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"number":7}`))
	}))
	defer srv.Close()

	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	resp, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{"state": "open"},
		Body:  map[string]any{"title": "hello", "count": 3},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/repos/kai/aos/issues" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotQuery != "state=open" {
		t.Errorf("query = %q, want state=open", gotQuery)
	}
	if gotAuth != "token s3cret" {
		t.Errorf("auth header = %q, want %q", gotAuth, "token s3cret")
	}
	// Body typing is preserved: count is a JSON number, not a string.
	if gotBody["title"] != "hello" || gotBody["count"] != float64(3) {
		t.Errorf("body = %v", gotBody)
	}
	if m, ok := resp.Decoded.(map[string]any); !ok || m["number"] != float64(7) {
		t.Errorf("decoded = %v", resp.Decoded)
	}
}

// TestExecutePrunesDeclaredReturnFields checks that `returns` narrows both
// Response.Decoded and Response.Raw, not just the decoded value.
func TestExecutePrunesDeclaredReturnFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"internal_note":"do not leak"}`))
	}))
	defer srv.Close()

	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.ReturnFields = []opcore.Field{{Name: "number", Type: "integer"}}
	resp, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{"state": "open"},
		Body:  map[string]any{"title": "hello"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if m, ok := resp.Decoded.(map[string]any); !ok || len(m) != 1 || m["number"] != float64(7) {
		t.Errorf("decoded = %v, want only {number:7}", resp.Decoded)
	}
	var gotRaw map[string]any
	if err := json.Unmarshal(resp.Raw, &gotRaw); err != nil {
		t.Fatalf("unmarshal Raw: %v", err)
	}
	if len(gotRaw) != 1 || gotRaw["number"] != float64(7) {
		t.Errorf("raw = %v, want only {number:7}; internal_note leaked", gotRaw)
	}
}

// TestExecuteFailWhenSeesFullResponseBeforePruning checks that fail-when can
// still reference a field a `returns` declaration goes on to drop.
func TestExecuteFailWhenSeesFullResponseBeforePruning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"number":7,"state":"closed"}`))
	}))
	defer srv.Close()

	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.ReturnFields = []opcore.Field{{Name: "number", Type: "integer"}}
	op.Desc.FailWhen = "state == 'closed'"
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{"state": "open"},
		Body:  map[string]any{"title": "hello"},
	})
	if err == nil {
		t.Fatal("expected fail-when to reject a response returns would have pruned `state` from")
	}
	if kind := kindOf(err); kind != "upstream_failed" {
		t.Errorf("kind = %q, want upstream_failed", kind)
	}
}

func TestQueryAliasMapsLocalInputToUpstreamParameter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.QueryFlags = []opcore.Field{{Name: "search_query", UpstreamName: "query", Type: "string"}}
	args := opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{"search_query": "platform engineer"},
		Body:  map[string]any{"title": "search"},
	}
	req, err := op.Preview(args)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !strings.HasSuffix(req.URL, "?query=platform+engineer") {
		t.Errorf("URL = %q, want aliased query parameter", req.URL)
	}
	if strings.Contains(req.URL, "search_query") {
		t.Errorf("URL leaked local query name: %q", req.URL)
	}

	args.Query["query"] = "duplicate"
	if _, err := op.Preview(args); kindOf(err) != "user_error" {
		t.Fatalf("ambiguous local and upstream inputs: kind = %q, want user_error (err=%v)", kindOf(err), err)
	}
}

func TestTypedQueryValuesSerializeRepeatedKeysInInputOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	minLimit, maxLimit := float64(1), float64(100)
	minAuthors, maxAuthors := 1, 25
	op.Desc.QueryFlags = []opcore.Field{
		{Name: "limit", Type: "integer", Minimum: &minLimit, Maximum: &maxLimit},
		{Name: "pinned", Type: "boolean"},
		{Name: "score", Type: "number"},
		{Name: "author_id", Type: "array", Items: "string", MinItems: &minAuthors, MaxItems: &maxAuthors},
		{Name: "enabled", Type: "array", Items: "boolean"},
		{Name: "page", Type: "array", Items: "integer"},
		{Name: "weight", Type: "array", Items: "number"},
		{Name: "search_query", UpstreamName: "query", Type: "string"},
	}
	req, err := op.Preview(opcore.Args{
		Path: map[string]string{"owner": "kai", "repo": "aos"},
		QueryValues: map[string]any{
			"limit":        25,
			"pinned":       true,
			"score":        1.5,
			"author_id":    []any{"second", "first", "third"},
			"enabled":      []bool{true, false},
			"page":         []int{3, 1, 2},
			"weight":       []float64{2.5, 1.25},
			"search_query": "platform",
		},
		Body: map[string]any{"title": "search"},
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	parsed, err := neturl.Parse(req.URL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	if got, want := parsed.Query()["author_id"], []string{"second", "first", "third"}; !reflect.DeepEqual(got, want) {
		t.Errorf("repeated author_id values = %v, want %v", got, want)
	}
	if got := parsed.Query().Get("limit"); got != "25" {
		t.Errorf("limit = %q, want 25", got)
	}
	if got := parsed.Query().Get("pinned"); got != "true" {
		t.Errorf("pinned = %q, want true", got)
	}
	if got := parsed.Query().Get("score"); got != "1.5" {
		t.Errorf("score = %q, want 1.5", got)
	}
	if got, want := parsed.Query()["enabled"], []string{"true", "false"}; !reflect.DeepEqual(got, want) {
		t.Errorf("boolean array = %v, want %v", got, want)
	}
	if got, want := parsed.Query()["page"], []string{"3", "1", "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("integer array = %v, want %v", got, want)
	}
	if got, want := parsed.Query()["weight"], []string{"2.5", "1.25"}; !reflect.DeepEqual(got, want) {
		t.Errorf("number array = %v, want %v", got, want)
	}
	if got := parsed.Query().Get("query"); got != "platform" {
		t.Errorf("aliased query = %q, want platform", got)
	}
	if parsed.Query().Has("search_query") {
		t.Errorf("URL leaked local query name: %q", req.URL)
	}
}

func TestTypedQueryValidationFailsBeforeUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid typed query must not reach upstream")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	minLimit, maxLimit := float64(1), float64(100)
	minAuthors, maxAuthors := 1, 2
	op.Desc.QueryFlags = []opcore.Field{
		{Name: "limit", Type: "integer", Required: true, Minimum: &minLimit, Maximum: &maxLimit},
		{Name: "author_id", Type: "array", Items: "string", MinItems: &minAuthors, MaxItems: &maxAuthors},
	}
	base := opcore.Args{
		Path: map[string]string{"owner": "kai", "repo": "aos"},
		Body: map[string]any{"title": "search"},
	}
	cases := map[string]map[string]any{
		"below minimum":    {"limit": 0},
		"above maximum":    {"limit": 101},
		"wrong scalar":     {"limit": true},
		"empty array":      {"limit": 10, "author_id": []string{}},
		"array too long":   {"limit": 10, "author_id": []string{"a", "b", "c"}},
		"wrong array item": {"limit": 10, "author_id": []any{"a", true}},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			args := base
			args.QueryValues = values
			if _, err := op.Execute(context.Background(), args); kindOf(err) != "user_error" {
				t.Fatalf("kind = %q, want user_error (err=%v)", kindOf(err), err)
			}
		})
	}
	if _, err := op.Execute(context.Background(), base); kindOf(err) != "user_error" {
		t.Fatalf("missing required query: kind = %q, want user_error (err=%v)", kindOf(err), err)
	}
}

func TestTypedQueryMutualExclusionFailsBeforeUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("mutually-exclusive query must not reach upstream")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.QueryFlags = []opcore.Field{
		{Name: "before", Type: "string"},
		{Name: "after", Type: "string"},
		{Name: "around", Type: "string"},
	}
	op.Desc.QueryExclusive = [][]string{{"before", "after", "around"}}
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:        map[string]string{"owner": "kai", "repo": "aos"},
		QueryValues: map[string]any{"before": "1", "after": "2"},
		Body:        map[string]any{"title": "search"},
	})
	if kindOf(err) != "user_error" {
		t.Fatalf("kind = %q, want user_error (err=%v)", kindOf(err), err)
	}
}

// The negative control for the dropped query gate: the safety property is the
// encoding, so assert the encoding rather than the refusal it replaced.
func TestTypedQueryRepeatedValueEncodesMetacharacters(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.QueryFlags = []opcore.Field{{Name: "author_id", Type: "array", Items: "string"}}
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:        map[string]string{"owner": "kai", "repo": "aos"},
		QueryValues: map[string]any{"author_id": []string{"safe", "bad;value"}},
		Body:        map[string]any{"title": "search"},
	})
	if err != nil {
		t.Fatalf("query metachar must no longer be refused: %v", err)
	}
	// Check each value segment, not the whole string: & separates the repeated
	// keys and is the encoder's own structure rather than a leaked value byte.
	for _, pair := range strings.Split(gotRaw, "&") {
		_, value, _ := strings.Cut(pair, "=")
		if strings.ContainsAny(value, policy.ShellMeta) {
			t.Fatalf("query value %q in %q leaked a byte from ShellMeta; every one must be percent-encoded", value, gotRaw)
		}
	}
	if !strings.Contains(gotRaw, "bad%3Bvalue") {
		t.Fatalf("raw query = %q, want the metacharacter percent-encoded as bad%%3Bvalue", gotRaw)
	}
}

// A repeated bare name carries no arity at length one (umbra#7013). The default
// rows are load bearing: every existing surface encodes against them.
func TestQueryArrayEncodingWireShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encode string
		values []string
		want   string
	}{
		{"default keeps one element bare", "", []string{"a"}, "author_id=a"},
		{"default repeats the bare name", "", []string{"a", "b"}, "author_id=a&author_id=b"},
		{"repeat is the default spelled out", "repeat", []string{"a"}, "author_id=a"},
		{"brackets state arity at one", "brackets", []string{"a"}, "author_id%5B%5D=a"},
		{"brackets repeat at two", "brackets", []string{"a", "b"}, "author_id%5B%5D=a&author_id%5B%5D=b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRaw string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotRaw = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			op := newTestOp(srv, tokenAuth("s3cret"), nil)
			op.Desc.QueryFlags = []opcore.Field{
				{Name: "author_id", Type: "array", Items: "string", ArrayEncode: tc.encode},
			}
			_, err := op.Execute(context.Background(), opcore.Args{
				Path:        map[string]string{"owner": "kai", "repo": "aos"},
				QueryValues: map[string]any{"author_id": tc.values},
				Body:        map[string]any{"title": "search"},
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if gotRaw != tc.want {
				t.Fatalf("raw query = %q, want %q", gotRaw, tc.want)
			}
		})
	}
}

// The suffix belongs to the wire, so it follows the aliased upstream name.
func TestQueryArrayBracketsApplyToAliasedName(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	op.Desc.QueryFlags = []opcore.Field{
		{Name: "ids", Type: "array", Items: "string", UpstreamName: "selectedRecordIds", ArrayEncode: "brackets"},
	}
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:        map[string]string{"owner": "kai", "repo": "aos"},
		QueryValues: map[string]any{"ids": []string{"rec1"}},
		Body:        map[string]any{"title": "search"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotRaw != "selectedRecordIds%5B%5D=rec1" {
		t.Fatalf("raw query = %q, want the aliased name with the brackets suffix", gotRaw)
	}
	if strings.Contains(gotRaw, "ids=") {
		t.Fatalf("raw query %q leaked the local input name", gotRaw)
	}
}

// Checked one byte at a time so a later widening of ShellMeta cannot quietly
// outrun the encoder that query values rely on instead of the gate.
func TestEveryShellMetaByteIsEncodedInAQueryValue(t *testing.T) {
	for _, b := range []byte(policy.ShellMeta) {
		raw := neturl.Values{"q": []string{string(b)}}.Encode()
		if strings.ContainsRune(raw, rune(b)) {
			t.Errorf("ShellMeta byte %q survives Encode as %q; the query gate could not be dropped for it", string(b), raw)
		}
	}
	// & and = carry the only structural meaning in a query string.
	for _, b := range []string{"&", "="} {
		raw := neturl.Values{"q": []string{b}}.Encode()
		if strings.Contains(strings.TrimPrefix(raw, "q="), b) {
			t.Errorf("structural byte %q survives Encode as %q", b, raw)
		}
	}
}

func TestQueryInputCannotUseBothArgsSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)
	_, err := op.Preview(opcore.Args{
		Path:        map[string]string{"owner": "kai", "repo": "aos"},
		Query:       map[string]string{"state": "open"},
		QueryValues: map[string]any{"state": "closed"},
		Body:        map[string]any{"title": "search"},
	})
	if kindOf(err) != "user_error" {
		t.Fatalf("kind = %q, want user_error (err=%v)", kindOf(err), err)
	}
}

func TestExecuteSelfGuardsShellMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("server must not be reached; the gate should have rejected first")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)

	// Gated with no verb.Wrap around the call, the whole point for a non-CLI
	// consumer. Path is gated because FillPath substitutes it unescaped.
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos`whoami`"},
		Query: map[string]string{"state": "open"},
		Body:  map[string]any{"title": "x"},
	})
	if kindOf(err) != "policy_denied" {
		t.Fatalf("path metachar: kind = %q, want policy_denied", kindOf(err))
	}
}

func TestExecuteRestrictScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("out-of-scope call must not fire")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), []guardfile.Restriction{{Param: "owner", Globs: []string{"kai"}}})

	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "someone-else", "repo": "aos"},
		Query: map[string]string{},
		Body:  map[string]any{"title": "x"},
	})
	if kindOf(err) != "policy_denied" {
		t.Fatalf("restrict: kind = %q, want policy_denied", kindOf(err))
	}
}

func TestExecuteRequiredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("call with a missing required field must not fire")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)

	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{},
		Body:  map[string]any{"count": 1}, // title (required) omitted
	})
	if kindOf(err) != "user_error" {
		t.Fatalf("required body: kind = %q, want user_error", kindOf(err))
	}
}

func TestExecuteNestedRequiredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("call with a missing nested required field must not fire")
	}))
	defer srv.Close()
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL:   srv.URL,
		Auth:      tokenAuth("s3cret"),
		Providers: valuesource.Merge(nil),
		Client:    srv.Client(),
	})
	op := opcore.Operation{RT: rt, Desc: opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/query",
		Leaf:   "query",
		BodyFlags: []opcore.Field{
			{
				Name:     "compositeQuery",
				Type:     "object",
				Required: true,
				Fields: []opcore.Field{
					{Name: "start", Type: "integer", Required: true},
					{Name: "end", Type: "integer", Required: true},
				},
			},
		},
	}}
	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{},
		Query: map[string]string{},
		Body:  map[string]any{"compositeQuery": map[string]any{"start": 1}},
	})
	if kindOf(err) != "user_error" {
		t.Fatalf("nested required: kind = %q, want user_error (err=%v)", kindOf(err), err)
	}
	if err != nil && !strings.Contains(err.Error(), "compositeQuery.end") {
		t.Fatalf("nested required error should name the missing path, got %v", err)
	}
}

func TestExecuteMappedBodyProjectsOnlyDeclaredStrings(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL:   srv.URL,
		Auth:      tokenAuth("s3cret"),
		Providers: valuesource.Merge(nil),
		Client:    srv.Client(),
	})
	op := opcore.Operation{RT: rt, Desc: opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/sendMessage",
		Leaf:   "create",
		BodyMappings: []opcore.BodyMapping{
			{SourcePath: []string{"commonAnnotations", "summary"}, Target: "text"},
			{SourcePath: []string{"commonLabels", "alertname"}, Target: "alert_name"},
		},
	}}

	_, err := op.Execute(context.Background(), opcore.Args{Body: map[string]any{
		"commonAnnotations": map[string]any{
			"summary": "API error rate is high",
			"secret":  "must-not-leak",
		},
		"commonLabels": map[string]any{"alertname": "api-errors"},
		"body":         "must-not-leak",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := map[string]any{"text": "API error rate is high", "alert_name": "api-errors"}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("outgoing body = %#v, want %#v", gotBody, want)
	}
}

func TestExecuteMappedBodyRejectsMissingOrNonStringWithoutFiring(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer srv.Close()
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL:   srv.URL,
		Auth:      tokenAuth("s3cret"),
		Providers: valuesource.Merge(nil),
		Client:    srv.Client(),
	})
	op := opcore.Operation{RT: rt, Desc: opcore.Descriptor{
		Method:       http.MethodPost,
		Path:         "/sendMessage",
		Leaf:         "create",
		BodyMappings: []opcore.BodyMapping{{SourcePath: []string{"commonAnnotations", "summary"}, Target: "text"}},
	}}
	cases := map[string]map[string]any{
		"missing":    {"commonAnnotations": map[string]any{}},
		"non-string": {"commonAnnotations": map[string]any{"summary": 42}},
		"non-object": {"commonAnnotations": "nope"},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := op.Execute(context.Background(), opcore.Args{Body: body})
			if kindOf(err) != "user_error" {
				t.Fatalf("kind = %q, want user_error (err=%v)", kindOf(err), err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

func TestExecuteMissingPathParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)

	_, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai"}, // repo missing
		Query: map[string]string{},
		Body:  map[string]any{"title": "x"},
	})
	if kindOf(err) != "user_error" {
		t.Fatalf("missing path param: kind = %q, want user_error", kindOf(err))
	}
}

func TestPreviewResolvesWithoutFiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("Preview must not fire a request")
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s3cret"), nil)

	req, err := op.Preview(opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{"state": "open"},
		Body:  map[string]any{"title": "hello"},
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %s", req.Method)
	}
	if req.URL != srv.URL+"/repos/kai/aos/issues?state=open" {
		t.Errorf("url = %q", req.URL)
	}
	var body map[string]any
	if err := json.Unmarshal(req.Body, &body); err != nil || body["title"] != "hello" {
		t.Errorf("body = %s (%v)", req.Body, err)
	}
}

func TestPreviewValueBaseStaysOffline(t *testing.T) {
	// A value-resolved base-url must never touch its provider during a preview.
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		Auth:         tokenAuth("s"),
		Providers:    valuesource.Merge(nil),
		BaseURLValue: guardfile.ValueChain{{Provider: "literal", Address: "https://example.test"}},
	})
	op := opcore.Operation{RT: rt, Desc: opcore.Descriptor{
		Method: http.MethodGet, Path: "/x", Leaf: "get",
	}}
	req, err := op.Preview(opcore.Args{Path: map[string]string{}, Query: map[string]string{}})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if want := "{base-url:"; len(req.URL) < len(want) || req.URL[:len(want)] != want {
		t.Errorf("preview url = %q, want a {base-url:...} placeholder", req.URL)
	}
}

func TestExecuteFixedBodyWins(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rt := opcore.NewRuntime(opcore.RuntimeConfig{
		BaseURL: srv.URL, Auth: tokenAuth("s"), Providers: valuesource.Merge(nil), Client: srv.Client(),
	})
	op := opcore.Operation{RT: rt, Desc: opcore.Descriptor{
		Method:     http.MethodPatch,
		Path:       "/repos/{owner}/{repo}",
		Leaf:       "archive",
		PathParams: []string{"owner", "repo"},
		FixedBody:  map[string]any{"archived": true},
	}}
	if _, err := op.Execute(context.Background(), opcore.Args{
		Path:  map[string]string{"owner": "kai", "repo": "aos"},
		Query: map[string]string{},
		Body:  map[string]any{"ignored": "value"}, // a fixed-body leaf owns its body
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(gotBody) != 1 || gotBody["archived"] != true {
		t.Errorf("fixed body = %v, want {archived:true}", gotBody)
	}
}

func TestExecuteUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	op := newTestOp(srv, tokenAuth("s"), nil)
	_, err := op.Execute(context.Background(), opcore.Args{
		Path: map[string]string{"owner": "kai", "repo": "aos"}, Query: map[string]string{},
		Body: map[string]any{"title": "x"},
	})
	if kindOf(err) != "upstream_failed" {
		t.Fatalf("403: kind = %q, want upstream_failed", kindOf(err))
	}
}

func TestExecuteInlineFailWhenUsesRequestBodyVariables(t *testing.T) {
	response := `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()

	op := newTestOp(srv, tokenAuth("s"), nil)
	op.Desc.BodyFlags = []opcore.Field{{
		Name: "labels", Type: "array", Items: "integer", Required: true,
	}}
	op.Desc.FailWhen = "length([?contains($labels, id)]) != length($labels)"
	args := opcore.Args{
		Path: map[string]string{"owner": "kai", "repo": "aos"},
		Body: map[string]any{"labels": []any{float64(346)}},
	}

	if _, err := op.Execute(context.Background(), args); kindOf(err) != "upstream_failed" {
		t.Fatalf("missing requested label: kind = %q, want upstream_failed (err=%v)", kindOf(err), err)
	}

	response = `[{"id":346}]`
	if _, err := op.Execute(context.Background(), args); err != nil {
		t.Fatalf("present requested label: %v", err)
	}
}

func TestDefaultScheme(t *testing.T) {
	cases := map[string]string{
		"forgejo.coilysiren.me/api/v1":         "https://forgejo.coilysiren.me/api/v1",
		"https://forgejo.coilysiren.me/api/v1": "https://forgejo.coilysiren.me/api/v1",
		"http://127.0.0.1:8080":                "http://127.0.0.1:8080",
		"":                                     "",
	}
	for in, want := range cases {
		if got := opcore.DefaultScheme(in); got != want {
			t.Errorf("DefaultScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMethodForVerb(t *testing.T) {
	cases := []struct {
		verb   string
		method string
		known  bool
	}{
		{"get", "GET", true},
		{"list", "GET", true},
		{"create", "POST", true},
		{"edit", "PATCH", true},
		{"delete", "DELETE", true},
		{"set", "PUT", true},
		{"search", "GET", true},
		{"list-runs", "GET", true},
		{"create-on-repo", "POST", true},
		{"comment", "POST", true},   // was reaching POST through the fallthrough
		{"pin", "POST", true},       // same, and the one guardfiles documented
		{"transfer", "POST", false}, // unknown verb -> sub-collection create default
	}
	for _, c := range cases {
		m, ok := opcore.MethodForVerb(c.verb)
		if m != c.method || ok != c.known {
			t.Errorf("MethodForVerb(%q) = (%q,%v), want (%q,%v)", c.verb, m, ok, c.method, c.known)
		}
	}
}

func TestArgBinderRoutesByLocation(t *testing.T) {
	leaf := opcore.Descriptor{
		Path:       "/repos/{owner}/{repo}/issues",
		PathParams: []string{"owner", "repo"},
		QueryFlags: []opcore.Field{{Name: "state"}, {Name: "search_query", UpstreamName: "query"}},
		BodyFlags:  []opcore.Field{{Name: "title"}},
	}
	b := opcore.NewArgBinder(leaf)
	if err := b.Bind(opcore.OwnerRepoArg, "kai/aos"); err != nil {
		t.Fatalf("bind owner-repo: %v", err)
	}
	if err := b.Bind("state", "open"); err != nil {
		t.Fatalf("bind state: %v", err)
	}
	if err := b.Bind("search_query", "platform"); err != nil {
		t.Fatalf("bind search query: %v", err)
	}
	if err := b.Bind("title", "hello"); err != nil {
		t.Fatalf("bind title: %v", err)
	}
	if err := b.RequireAllPaths(); err != nil {
		t.Fatalf("requireAllPaths: %v", err)
	}
	if opcore.FillPath(leaf.Path, b.PathVals) != "/repos/kai/aos/issues" {
		t.Errorf("path = %q", opcore.FillPath(leaf.Path, b.PathVals))
	}
	if b.Query.Get("state") != "open" {
		t.Errorf("query state = %q", b.Query.Get("state"))
	}
	if b.Query.Get("query") != "platform" || b.Query.Has("search_query") {
		t.Errorf("aliased query = %q, local present = %v", b.Query.Get("query"), b.Query.Has("search_query"))
	}
	if b.BodyObj["title"] != "hello" {
		t.Errorf("body title = %v", b.BodyObj["title"])
	}

	// A malformed owner-repo value fails closed.
	if err := opcore.NewArgBinder(leaf).Bind(opcore.OwnerRepoArg, "no-slash"); err == nil {
		t.Errorf("owner-repo without a slash should error")
	}
}
