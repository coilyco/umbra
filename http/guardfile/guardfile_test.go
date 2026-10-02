package guardfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseFixture(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "forgejo.kdl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got, want := gf.Group, []string{"ward", "ops", "forgejo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Group = %v, want %v", got, want)
	}
	if got, want := gf.Spec, "forgejo.swagger.v1.json"; got != want {
		t.Errorf("Spec = %q, want %q", got, want)
	}
	if got, want := gf.BaseURL, "https://forgejo.example/api/v1"; got != want {
		t.Errorf("BaseURL = %q, want %q", got, want)
	}

	wantAuth := Auth{Scheme: "header-token", Header: "Authorization", Prefix: "token ", Value: ValueChain{{Provider: "ssm", Address: "/forgejo/api-token"}}}
	if !reflect.DeepEqual(gf.Auth, wantAuth) {
		t.Errorf("Auth = %+v, want %+v", gf.Auth, wantAuth)
	}

	wantGrants := []Grant{
		{Modal: "can", Verb: "get", Resource: "repos", Op: "repoGet"},
		{Modal: "can", Verb: "create", Resource: "repos", Op: "createCurrentUserRepo"},
		{Modal: "can", Verb: "delete", Resource: "repos", Op: "repoDelete"},
	}
	if !reflect.DeepEqual(gf.Grants, wantGrants) {
		t.Errorf("Grants = %+v, want %+v", gf.Grants, wantGrants)
	}
}

// TestParseDescription checks the top-level `description` node parses into
// Guardfile.Description, is optional, and fails closed on an empty/malformed value.
func TestParseDescription(t *testing.T) {
	base := `wrap ward ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token { header Authorization; value ssm "/forgejo/api-token" }
    can get repos
}`

	t.Run("top-level description parses", func(t *testing.T) {
		src := `description "Forgejo ops surface: scoped read/write over the coily* orgs."` + "\n" + base
		gf, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if gf.Description != "Forgejo ops surface: scoped read/write over the coily* orgs." {
			t.Errorf("Description = %q", gf.Description)
		}
	})

	t.Run("absent description is empty", func(t *testing.T) {
		gf, err := Parse([]byte(base))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if gf.Description != "" {
			t.Errorf("Description = %q, want empty", gf.Description)
		}
	})

	t.Run("empty description fails closed", func(t *testing.T) {
		src := `description ""` + "\n" + base
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "non-empty") {
			t.Fatalf("want non-empty error, got %v", err)
		}
	})

	t.Run("two-arg description fails closed", func(t *testing.T) {
		src := `description "a" "b"` + "\n" + base
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "description") {
			t.Fatalf("want description arity error, got %v", err)
		}
	})
}

// TestBareTokensAreStrings asserts the flat policy body and the dotted spec
// filename parse as bare identifiers, so authors never quote outside the header.
func TestBareTokensAreStrings(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
    spec forgejo.swagger.v1.json
    auth header-token {
        header Authorization
        value ssm "/forgejo/api-token"
    }
    can delete labels created-by-me { op "labelDelete" }
}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gf.Spec != "forgejo.swagger.v1.json" {
		t.Errorf("dotted bare spec did not round-trip: %q", gf.Spec)
	}
	want := Grant{Modal: "can", Verb: "delete", Resource: "labels", Qualifiers: []string{"created-by-me"}, Op: "labelDelete"}
	if len(gf.Grants) != 1 || !reflect.DeepEqual(gf.Grants[0], want) {
		t.Errorf("flat qualifier sentence = %+v, want %+v", gf.Grants, want)
	}
}

// TestGrantDescribeAnnotation asserts a grant-body `describe "..."` child flows
// into Grant.Describe, the per-grant slot that enriches the thin upstream spec.
func TestGrantDescribeAnnotation(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
	    spec s
	    auth header-token { header H; value ssm S }
	    can delete repos {
	        op "repoDelete"
	        describe "irreversible: deletes the repo and all its data"
	    }
	}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Grant{Modal: "can", Verb: "delete", Resource: "repos", Op: "repoDelete", Describe: "irreversible: deletes the repo and all its data"}
	if len(gf.Grants) != 1 || !reflect.DeepEqual(gf.Grants[0], want) {
		t.Errorf("grant with describe = %+v, want %+v", gf.Grants, want)
	}
}

// TestGrantProperties asserts KDL key=value properties land in Grant.Props,
// distinct from positional bareword qualifiers.
func TestGrantProperties(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
	    spec s
	    auth header-token { header H; value ssm S }
	    can delete repos org="coilyco-flight-deck" { op "repoDelete" }
	}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Grant{Modal: "can", Verb: "delete", Resource: "repos", Op: "repoDelete", Props: map[string]string{"org": "coilyco-flight-deck"}}
	if len(gf.Grants) != 1 || !reflect.DeepEqual(gf.Grants[0], want) {
		t.Errorf("grant with org property = %+v, want %+v", gf.Grants, want)
	}
}

// TestParseGrantWithoutOp asserts a `can` parses with no `op` binding: Op is
// optional at the parser layer (resolved by convention downstream), not an error.
func TestParseGrantWithoutOp(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
	    spec s
	    auth header-token { header H; value ssm S }
	    can get repo
	}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Grant{Modal: "can", Verb: "get", Resource: "repo"}
	if len(gf.Grants) != 1 || !reflect.DeepEqual(gf.Grants[0], want) {
		t.Errorf("grant without op = %+v, want %+v", gf.Grants, want)
	}
}

// TestParseWildcardResource asserts the `"*"` resource sentinel parses with the
// Wildcard flag set, on both an allow and a deny modal, carrying its block fields.
func TestParseWildcardResource(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
	    spec s
	    auth header-token { header H; value ssm S }
	    can get "*"
	    never delete "*" { message "deletes are frozen" }
	}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Grant{
		{Modal: "can", Verb: "get", Resource: "*", Wildcard: true},
		{Modal: "never", Verb: "delete", Resource: "*", Wildcard: true, Message: "deletes are frozen"},
	}
	if !reflect.DeepEqual(gf.Grants, want) {
		t.Errorf("wildcard grants = %+v, want %+v", gf.Grants, want)
	}
}

// TestParseAction asserts the complex-action grammar round-trips: inputs, the
// poll primitive with its bounds, the multiline `until`, and `fail-when`.
func TestParseAction(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
    spec s
    auth header-token { header H; value ssm S }
    can list tasks { op "ListActionTasks" }

    action ci-watch {
        describe "Watch a CI run to completion."
        input repo { positional; required; help "owner/name" }
        input run  { flag; help "run number" }
        poll list tasks {
            args { owner-repo $repo }
            until """
                length([?run_number==$run && status!='success'
                        && status!='failure']) == ` + "`0`" + `
                """
            every "10s"
            timeout "30m"
            as run_tasks
        }
        fail-when "length(run_tasks[?status=='failure']) > ` + "`0`" + `"
    }
}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(gf.Actions) != 1 {
		t.Fatalf("Actions = %d, want 1", len(gf.Actions))
	}
	act := gf.Actions[0]
	if act.Name != "ci-watch" {
		t.Errorf("Name = %q, want ci-watch", act.Name)
	}
	if act.Describe != "Watch a CI run to completion." {
		t.Errorf("Describe = %q", act.Describe)
	}
	wantInputs := []Input{
		{Name: "repo", Positional: true, Required: true, Help: "owner/name"},
		{Name: "run", Positional: false, Required: false, Help: "run number"},
	}
	if !reflect.DeepEqual(act.Inputs, wantInputs) {
		t.Errorf("Inputs = %+v, want %+v", act.Inputs, wantInputs)
	}
	if act.Poll == nil {
		t.Fatal("Poll is nil")
	}
	if act.Poll.Verb != "list" || act.Poll.Resource != "tasks" {
		t.Errorf("Poll target = %q %q, want list tasks", act.Poll.Verb, act.Poll.Resource)
	}
	wantArgs := []ArgBind{{Name: "owner-repo", Value: "$repo"}}
	if !reflect.DeepEqual(act.Poll.Args, wantArgs) {
		t.Errorf("Poll.Args = %+v, want %+v", act.Poll.Args, wantArgs)
	}
	if act.Poll.Every != "10s" || act.Poll.Timeout != "30m" || act.Poll.As != "run_tasks" {
		t.Errorf("Poll bounds = every %q timeout %q as %q", act.Poll.Every, act.Poll.Timeout, act.Poll.As)
	}
	// The multiline `until` keeps its inner alignment, dedented to the close.
	if !strings.Contains(act.Poll.Until, "length([?run_number==$run") || !strings.Contains(act.Poll.Until, "status!='failure'") {
		t.Errorf("Until did not round-trip the multiline expression:\n%s", act.Poll.Until)
	}
	if act.FailWhen != "length(run_tasks[?status=='failure']) > `0`" {
		t.Errorf("FailWhen = %q", act.FailWhen)
	}
}

// TestParseFetch asserts the fetch overlay grammar round-trips: method/path,
// env-backed header templates, raw output, and the `first input` sugar.
func TestParseFetch(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
    base-url "https://forgejo.example/api/v1"
    fetch "actions logs" {
        method "GET"
        path "/repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs"
        output "raw"
        env FORGEJO_TOKEN {
            value ssm "/forgejo/token"
        }
        header "Authorization" "token ${FORGEJO_TOKEN}"
        header "Accept" "text/plain"
        when first input matches coily*
    }
}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(gf.Fetches) != 1 {
		t.Fatalf("Fetches = %d, want 1", len(gf.Fetches))
	}
	want := Fetch{
		Name:   "actions logs",
		Leaf:   "actions-logs",
		Method: "GET",
		Path:   "/repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs",
		Output: "raw",
		Env: []FetchEnv{{
			Name:  "FORGEJO_TOKEN",
			Value: ValueChain{{Provider: "ssm", Address: "/forgejo/token"}},
		}},
		Headers: []FetchHeader{
			{Name: "Authorization", Value: "token ${FORGEJO_TOKEN}"},
			{Name: "Accept", Value: "text/plain"},
		},
		Whens: []FetchWhen{{Selector: "arg0", Globs: []string{"coily*"}}},
	}
	if !reflect.DeepEqual(gf.Fetches[0], want) {
		t.Errorf("Fetch = %+v, want %+v", gf.Fetches[0], want)
	}
	gotProviders := gf.Providers()
	if len(gotProviders) != 1 || gotProviders[0] != "ssm" {
		t.Errorf("Providers = %v, want [ssm]", gotProviders)
	}
}

// TestParseFetchFailsClosed asserts the fetch grammar rejects unknown fields
// and non-raw output instead of silently dropping them.
func TestParseFetchFailsClosed(t *testing.T) {
	base := `wrap ward ops forgejo {
    base-url "https://forgejo.example/api/v1"
    fetch "actions logs" {
        method "GET"
        path "/repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs"
        output "raw"
        env FORGEJO_TOKEN { value ssm "/forgejo/token" }
        header "Authorization" "token ${FORGEJO_TOKEN}"
    }
}`
	for name, src := range map[string]string{
		"unknown child": `wrap ward ops forgejo {
    base-url "https://forgejo.example/api/v1"
    fetch "actions logs" {
        method "GET"
        path "/repos/{owner}/{repo}/actions/runs/{run}/jobs/{job}/attempt/{attempt}/logs"
        output "raw"
        env FORGEJO_TOKEN { value ssm "/forgejo/token" }
        header "Authorization" "token ${FORGEJO_TOKEN}"
        nope "x"
    }
}`,
		"bad output": strings.Replace(base, "output \"raw\"", "output \"yaml\"", 1),
		"bad when":   strings.Replace(base, "header \"Authorization\" \"token ${FORGEJO_TOKEN}\"", "header \"Authorization\" \"token ${MISSING}\"", 1),
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected a fail-closed parse error", name)
		}
	}
}

// TestParseInputDefault asserts an `input { default "<jmespath>" }` slot
// round-trips onto Input.Default, the pre-flight latest-run defaulting binding.
func TestParseInputDefault(t *testing.T) {
	src := []byte(`wrap ward ops forgejo {
    spec s
    auth header-token { header H; value ssm S }
    can list tasks { op "ListActionTasks" }

    action ci-watch {
        input repo { positional; required; help "owner/name" }
        input run  { flag; default "max(workflow_runs[].run_number)"; help "run number (default: latest)" }
        poll list tasks {
            args { owner-repo $repo }
            until "x"
            every "10s"
            timeout "30m"
            as run_tasks
        }
    }
}`)
	gf, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wantInputs := []Input{
		{Name: "repo", Positional: true, Required: true, Help: "owner/name"},
		{Name: "run", Positional: false, Default: "max(workflow_runs[].run_number)", Help: "run number (default: latest)"},
	}
	if !reflect.DeepEqual(gf.Actions[0].Inputs, wantInputs) {
		t.Errorf("Inputs = %+v, want %+v", gf.Actions[0].Inputs, wantInputs)
	}
}

// TestParseMountAction asserts the two-arg `action <verb> <resource>` form
// parses into a mount action: Name synthesized, MountVerb/MountResource set.
func TestParseMountAction(t *testing.T) {
	gf, err := Parse([]byte(`wrap ward ops forgejo {
		spec s
		auth header-token { header H; value ssm S }
		can view issue { op "issueGetIssue" }
		can list issue-comment { op "issueGetComments" }
		action view issue {
			describe "View an issue with its full comment thread."
			input source { positional; required; help "owner/name" }
			input index  { positional; required; help "issue number" }
			call view issue {
				args { owner-repo $source; index $index }
				as issue
			}
			call list issue-comment {
				args { owner-repo $source; index $index }
				as comments
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(gf.Actions) != 1 {
		t.Fatalf("Actions = %d, want 1", len(gf.Actions))
	}
	act := gf.Actions[0]
	if !act.IsMount() {
		t.Fatalf("IsMount() = false, want true")
	}
	if act.MountVerb != "view" || act.MountResource != "issue" {
		t.Errorf("mount target = %q %q, want view issue", act.MountVerb, act.MountResource)
	}
	if act.Name != "view-issue" {
		t.Errorf("synthesized Name = %q, want view-issue", act.Name)
	}
	if len(act.Calls) != 2 || act.Calls[0].As != "issue" || act.Calls[1].As != "comments" {
		t.Errorf("calls = %+v, want two with as issue/comments", act.Calls)
	}
}

// TestParseMountActionFailsClosed rejects a three-arg action header.
func TestParseMountActionFailsClosed(t *testing.T) {
	src := `wrap ward ops forgejo {
		spec s
		auth header-token { header H; value ssm S }
		can view issue { op "issueGetIssue" }
		action view issue extra {
			call view issue { args { owner-repo $source } as issue }
		}
	}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected an error for a three-arg action header, got nil")
	}
}

// TestParseAuthSchemes asserts the three auth schemes round-trip: header-token,
// bearer (Authorization + "Bearer " implied), and query-param dual-secret.
func TestParseAuthSchemes(t *testing.T) {
	bearer, err := Parse([]byte(`wrap w ops tailscale {
		spec s
		auth bearer { value ssm "/tailscale/api-key" }
		can list devices { op "listTailnetDevices" }
	}`))
	if err != nil {
		t.Fatalf("bearer parse: %v", err)
	}
	want := Auth{Scheme: "bearer", Header: "Authorization", Prefix: "Bearer ", Value: ValueChain{{Provider: "ssm", Address: "/tailscale/api-key"}}}
	if !reflect.DeepEqual(bearer.Auth, want) {
		t.Errorf("bearer auth = %+v, want %+v", bearer.Auth, want)
	}

	qp, err := Parse([]byte(`wrap w ops trello {
		spec s
		auth query-param {
			param key { value ssm "/trello/api-key" }
			param token { value ssm "/trello/api-token" }
		}
		can create cards { op "post-cards" }
	}`))
	if err != nil {
		t.Fatalf("query-param parse: %v", err)
	}
	if qp.Auth.Scheme != "query-param" || len(qp.Auth.Params) != 2 {
		t.Fatalf("query-param auth = %+v", qp.Auth)
	}
	wantParams := []QueryAuthParam{
		{Name: "key", Value: ValueChain{{Provider: "ssm", Address: "/trello/api-key"}}},
		{Name: "token", Value: ValueChain{{Provider: "ssm", Address: "/trello/api-token"}}},
	}
	if !reflect.DeepEqual(qp.Auth.Params, wantParams) {
		t.Errorf("query-param params = %+v, want %+v", qp.Auth.Params, wantParams)
	}
}

// TestParseBaseURLForms covers both base-url shapes: the committed string and the
// `{ value <provider> }` block for an opaque host resolved at request time.
func TestParseBaseURLForms(t *testing.T) {
	str, err := Parse([]byte(`wrap w ops forgejo {
		spec s
		base-url "forgejo.example/api/v1"
		auth bearer { value ssm "/x" }
		can get session { op o }
	}`))
	if err != nil {
		t.Fatalf("string base-url parse: %v", err)
	}
	if str.BaseURL != "forgejo.example/api/v1" || !str.BaseURLValue.IsZero() {
		t.Errorf("string form: BaseURL=%q BaseURLValue=%+v", str.BaseURL, str.BaseURLValue)
	}

	ssm, err := Parse([]byte(`wrap w ops owui {
		spec s
		base-url { value ssm "/coilysiren/open-webui/url" }
		auth bearer { value ssm "/x" }
		can get session { op o }
	}`))
	if err != nil {
		t.Fatalf("ssm base-url parse: %v", err)
	}
	if ssm.BaseURL != "" || !reflect.DeepEqual(ssm.BaseURLValue, ValueChain{{Provider: "ssm", Address: "/coilysiren/open-webui/url"}}) {
		t.Errorf("value form: BaseURL=%q BaseURLValue=%+v", ssm.BaseURL, ssm.BaseURLValue)
	}
}

// TestParseActionFailsClosed asserts the action grammar rejects every malformed
// or reserved shape, never silently dropping a node.
func TestParseActionFailsClosed(t *testing.T) {
	hdr := "wrap w {\n spec s\n auth header-token { header H; value ssm S }\n"
	cases := map[string]string{
		"no poll":                hdr + `action a { describe "x" } }`,
		"poll missing every":     hdr + `action a { poll list tasks { until "x"; timeout "1m"; as r } } }`,
		"poll missing until":     hdr + `action a { poll list tasks { every "1s"; timeout "1m"; as r } } }`,
		"poll missing as":        hdr + `action a { poll list tasks { until "x"; every "1s"; timeout "1m" } } }`,
		"two polls":              hdr + `action a { poll list tasks { until "x"; every "1s"; timeout "1m"; as r }; poll list tasks { until "y"; every "1s"; timeout "1m"; as q } } }`,
		"input no kind":          hdr + `action a { input repo { required }; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
		"required+default":       hdr + `action a { input run { flag; required; default "x" }; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
		"reserved each":          hdr + `action a { each "x" { }; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
		"reserved emit":          hdr + `action a { poll list tasks { until "x"; every "1s"; timeout "1m"; as r; emit "x" } } }`,
		"unknown poll node":      hdr + `action a { poll list tasks { until "x"; every "1s"; timeout "1m"; as r; bogus "x" } } }`,
		"reject-empty with arg":  hdr + `action a { reject-empty "x"; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
		"reject-empty with prop": hdr + `action a { reject-empty k="v"; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
		"duplicate reject-empty": hdr + `action a { reject-empty; reject-empty; poll list tasks { until "x"; every "1s"; timeout "1m"; as r } } }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestParseFailsClosed(t *testing.T) {
	cases := map[string]string{
		"unknown node": `wrap ward ops forgejo {
			spec s
			auth header-token { header H; value ssm S }
			allow read repos
		}`,
		"retired doc-link": `wrap ward ops forgejo {
			spec s
			auth header-token { header H; value ssm S }
			can read repos
			doc-link "x"
		}`,
		"missing spec": `wrap ward ops forgejo {
			auth header-token { header H; value ssm S }
			can read repos
		}`,
		"missing auth": `wrap ward ops forgejo {
			spec s
			can read repos
		}`,
		"no group": `wrap {
			spec s
			auth header-token { header H; value ssm S }
		}`,
		"grant missing resource": `wrap ward ops forgejo {
			spec s
			auth header-token { header H; value ssm S }
			can read
		}`,
		"unsupported auth scheme": `wrap ward ops forgejo {
			spec s
			auth oauth2 { value ssm S }
		}`,
		"bearer needs value": `wrap ward ops forgejo {
			spec s
			auth bearer { }
		}`,
		"query-param needs a param": `wrap ward ops forgejo {
			spec s
			auth query-param { }
		}`,
		"unknown grant-body node": `wrap ward ops forgejo {
			spec s
			auth header-token { header H; value ssm S }
			can delete repos { explain "nope" }
		}`,
		"base-url block unknown field": `wrap ward ops owui {
			spec s
			base-url { host "h" }
			auth bearer { value ssm S }
			can get session { op o }
		}`,
		"base-url block needs value": `wrap ward ops owui {
			spec s
			base-url { }
			auth bearer { value ssm S }
			can get session { op o }
		}`,
		"base-url both forms": `wrap ward ops owui {
			spec s
			base-url "h"
			base-url { value ssm "/x" }
			auth bearer { value ssm S }
			can get session { op o }
		}`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// TestParseValueProviders asserts the `value <provider> "addr"` grammar binds
// distinct providers across auth and base-url, and that Providers() reports them.
func TestParseValueProviders(t *testing.T) {
	gf, err := Parse([]byte(`wrap w ops owui {
		spec s
		base-url { value tailscale "open-webui" }
		auth header-token { header Authorization; value env "OWUI_TOKEN" }
		can get session { op o }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(gf.Auth.Value, ValueChain{{Provider: "env", Address: "OWUI_TOKEN"}}) {
		t.Errorf("auth value = %+v", gf.Auth.Value)
	}
	if !reflect.DeepEqual(gf.BaseURLValue, ValueChain{{Provider: "tailscale", Address: "open-webui"}}) {
		t.Errorf("base-url value = %+v", gf.BaseURLValue)
	}
	got := gf.Providers()
	want := map[string]bool{"env": true, "tailscale": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("Providers() = %v, want env+tailscale", got)
	}
}

// TestParseValueArityFailsClosed asserts a `value` node needs exactly a provider
// and an address; any other arity is a parse error.
func TestParseValueArityFailsClosed(t *testing.T) {
	cases := map[string]string{
		"value missing address": `wrap w { spec s
			auth header-token { header H; value ssm } }`,
		"value too many args": `wrap w { spec s
			auth header-token { header H; value ssm "/a" "/b" } }`,
		"base-url value bare": `wrap w ops owui { spec s
			base-url { value ssm }
			auth bearer { value ssm "/x" }
			can get session { op o } }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// TestParseValueChainForm asserts the children-block fallback list parses to an
// ordered ValueChain and that Providers() dedups across every chain.
func TestParseValueChainForm(t *testing.T) {
	gf, err := Parse([]byte(`wrap w ops forgejo {
		spec s
		base-url {
			value {
				env FORGEJO_BASE_URL
				ssm "/forgejo/base-url"
			}
		}
		auth header-token {
			header Authorization
			value {
				env FORGEJO_API_TOKEN
				ssm "/forgejo/coilyco-ops/api-token"
			}
		}
		can get session { op o }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantAuth := ValueChain{
		{Provider: "env", Address: "FORGEJO_API_TOKEN"},
		{Provider: "ssm", Address: "/forgejo/coilyco-ops/api-token"},
	}
	if !reflect.DeepEqual(gf.Auth.Value, wantAuth) {
		t.Errorf("auth chain = %+v, want %+v", gf.Auth.Value, wantAuth)
	}
	wantBase := ValueChain{
		{Provider: "env", Address: "FORGEJO_BASE_URL"},
		{Provider: "ssm", Address: "/forgejo/base-url"},
	}
	if !reflect.DeepEqual(gf.BaseURLValue, wantBase) {
		t.Errorf("base-url chain = %+v, want %+v", gf.BaseURLValue, wantBase)
	}
	// env appears in both chains; Providers() reports each distinct provider once.
	got := gf.Providers()
	want := map[string]bool{"env": true, "ssm": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("Providers() = %v, want env+ssm deduped", got)
	}
}

// TestParseValueChainQueryParam asserts each query-param secret takes its own
// fallback list independently.
func TestParseValueChainQueryParam(t *testing.T) {
	gf, err := Parse([]byte(`wrap w ops trello {
		spec s
		auth query-param {
			param key { value { env TRELLO_KEY; ssm "/trello/key" } }
			param token { value ssm "/trello/token" }
		}
		can create cards { op "post-cards" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	wantParams := []QueryAuthParam{
		{Name: "key", Value: ValueChain{{Provider: "env", Address: "TRELLO_KEY"}, {Provider: "ssm", Address: "/trello/key"}}},
		{Name: "token", Value: ValueChain{{Provider: "ssm", Address: "/trello/token"}}},
	}
	if !reflect.DeepEqual(gf.Auth.Params, wantParams) {
		t.Errorf("params = %+v, want %+v", gf.Auth.Params, wantParams)
	}
}

// TestParseValueChainFailsClosed asserts the fallback-block grammar rejects every
// malformed shape at parse time (empty, missing address, mixed form, nested source).
func TestParseValueChainFailsClosed(t *testing.T) {
	cases := map[string]string{
		"empty block": `wrap w { spec s
			auth header-token { header H; value { } } }`,
		"source missing address": `wrap w { spec s
			auth header-token { header H; value { env; ssm "/a" } } }`,
		"mixed inline and block": `wrap w { spec s
			auth header-token { header H; value ssm "/a" { env FOO } } }`,
		"source with children": `wrap w { spec s
			auth header-token { header H; value { env FOO { ssm "/a" } } } }`,
		"source with properties": `wrap w { spec s
			auth header-token { header H; value { env addr="/a" } } }`,
		"base-url empty block": `wrap w ops owui { spec s
			base-url { value { } }
			auth bearer { value ssm "/x" }
			can get session { op o } }`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Errorf("expected error for %s, got nil", name)
			}
		})
	}
}

// TestRemovedDeploymentSyntaxFailsClosed keeps legacy guardfiles from silently
// changing behavior after deployment policy left the parser.
func TestRemovedDeploymentSyntaxFailsClosed(t *testing.T) {
	base := "wrap ward ops eco {\n" +
		"spec s\nauth header-token { header H; value ssm S }\n" +
		"can promote deploy { op \"deployPromote\" }\ncan get health { op \"healthGet\" }\n"
	cases := map[string]string{
		"canary":     base + `action a { call promote deploy {}; canary get health { every "5s" } }` + "\n}",
		"compensate": base + `action a { call promote deploy { compensate delete snapshot {} } }` + "\n}",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "no longer supported") {
				t.Errorf("Parse error = %v, want clear removed-syntax diagnostic", err)
			}
		})
	}
}

// TestParseProviderDecl proves `provider <name> { exec ... }` parses, and that a
// bare numeric flag survives as its literal token rather than a debug repr.
func TestParseProviderDecl(t *testing.T) {
	gf, err := Parse([]byte(`wrap ward ops x {
		spec x.openapi.json
		base-url "https://example.test/api"
		auth bearer { value env "TOK" }
		provider ssm {
			exec aws ssm get-parameter --with-decryption --output text --name
		}
		provider tailscale {
			exec tailscale ip -4
		}
		can get thing { op "get_thing" }
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(gf.ProviderDecls) != 2 {
		t.Fatalf("ProviderDecls = %d, want 2", len(gf.ProviderDecls))
	}
	if got := gf.ProviderDecls[0].Name; got != "ssm" {
		t.Errorf("first provider = %q, want ssm", got)
	}
	// `-4` lexes as a KDL Int; it must reach argv as "-4", not "<kdl.Int -4>".
	ts := gf.ProviderDecls[1].Exec
	if len(ts) != 3 || ts[2] != "-4" {
		t.Errorf("tailscale exec = %#v, want [tailscale ip -4] with a literal -4", ts)
	}
}

// TestParseProviderFailsClosed proves a provider with no exec, or an unknown
// child, is a parse error rather than a silently unresolvable value source.
func TestParseProviderFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"no exec":       `provider ssm { }`,
		"unknown child": `provider ssm { shell "aws ssm get-parameter" }`,
	} {
		src := []byte(`wrap ward ops x {
			spec x.openapi.json
			base-url "https://example.test/api"
			auth bearer { value env "TOK" }
			` + body + `
			can get thing { op "get_thing" }
		}`)
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected a fail-closed parse error, got nil", name)
		}
	}
}

// TestParseInputArray asserts `array` round-trips onto Input.Array, and that the
// two shapes it cannot mean are refused at parse time rather than at call time.
func TestParseInputArray(t *testing.T) {
	action := func(inputBlock string) []byte {
		return []byte(`wrap ward ops forgejo {
    spec s
    auth header-token { header H; value ssm S }
    can create issue { op "issueCreateIssue" }

    action create issue {
        input repo { positional; required; help "owner/name" }
        ` + inputBlock + `
        call create issue { args { owner-repo $repo } }
    }
}`)
	}

	gf, err := Parse(action(`input labels { flag; required; array; help "label id, repeatable" }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Input{Name: "labels", Required: true, Array: true, Help: "label id, repeatable"}
	if got := gf.Actions[0].Inputs[1]; !reflect.DeepEqual(got, want) {
		t.Errorf("Input = %+v, want %+v", got, want)
	}

	for name, block := range map[string]string{
		"positional list":   `input labels { positional; array }`,
		"defaulted list":    `input labels { flag; array; default "x" }`,
		"unknown item type": `input labels { flag; arrays }`,
	} {
		if _, err := Parse(action(block)); err == nil {
			t.Errorf("%s: expected a fail-closed parse error", name)
		}
	}
}

// TestParseInputMatches asserts a constraint round-trips with its message, that
// several stack, and that malformed spellings fail closed at parse.
func TestParseInputMatches(t *testing.T) {
	action := func(inputBlock string) []byte {
		return []byte(`wrap ward ops forgejo {
    spec s
    auth header-token { header H; value ssm S }
    can create issue { op "issueCreateIssue" }

    action create issue {
        input repo { positional; required; help "owner/name" }
        ` + inputBlock + `
        call create issue { args { owner-repo $repo } }
    }
}`)
	}

	gf, err := Parse(action(`input labels {
        flag
        required
        array
        matches "priority/*" message="no priority label"
        matches "autonomy/headless" "autonomy/epic"
    }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Input{Name: "labels", Required: true, Array: true, Matches: []InputMatch{
		{Globs: []string{"priority/*"}, Message: "no priority label"},
		{Globs: []string{"autonomy/headless", "autonomy/epic"}},
	}}
	if got := gf.Actions[0].Inputs[1]; !reflect.DeepEqual(got, want) {
		t.Errorf("Input = %+v, want %+v", got, want)
	}

	for name, block := range map[string]string{
		"no glob":            `input labels { flag; matches }`,
		"empty glob":         `input labels { flag; matches "" }`,
		"empty glob in list": `input labels { flag; matches "a" "" }`,
		"unknown property":   `input labels { flag; matches "a" msg="x" }`,
	} {
		if _, err := Parse(action(block)); err == nil {
			t.Errorf("%s: expected a fail-closed parse error", name)
		}
	}
}

// TestParseRejectEmpty pins the marker and that it is off unless written: a
// parser setting it unconditionally would pass the first half alone.
func TestParseRejectEmpty(t *testing.T) {
	hdr := "wrap w {\n spec s\n auth header-token { header H; value ssm S }\n"
	step := `poll list tasks { until "x"; every "1s"; timeout "1m"; as r }`

	gf, err := Parse([]byte(hdr + `action a { reject-empty; ` + step + ` } }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !gf.Actions[0].RejectEmpty {
		t.Fatal("`reject-empty` was written and did not survive the parse")
	}

	gf, err = Parse([]byte(hdr + `action a { ` + step + ` } }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if gf.Actions[0].RejectEmpty {
		t.Fatal("RejectEmpty set on an action that never declared it")
	}
}
