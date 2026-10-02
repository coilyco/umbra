package guardfile_test

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	kdl "github.com/calico32/kdl-go"

	"github.com/coilyco/umbra/http/guardfile"
	"github.com/coilyco/umbra/http/opcore"
)

// dump renders a KDL tree canonically, so equal text means an equal tree.
// sortSiblings also ignores node order, for TOML, which writes scalars first.
func dump(t *testing.T, src string, sortSiblings bool) string {
	t.Helper()
	doc, err := kdl.ParseString(src)
	if err != nil {
		t.Fatalf("parse KDL: %v\n%s", err, src)
	}
	var render func(nodes []*kdl.Node, depth int) string
	render = func(nodes []*kdl.Node, depth int) string {
		parts := make([]string, 0, len(nodes))
		for _, n := range nodes {
			var b strings.Builder
			fmt.Fprintf(&b, "%s%s", strings.Repeat(" ", depth), n.Name())
			for _, a := range n.Arguments() {
				fmt.Fprintf(&b, " %v:%#v", a.Kind(), a.RawValue())
			}
			keys := make([]string, 0, len(n.Properties()))
			for k := range n.Properties() {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := n.Properties()[k]
				fmt.Fprintf(&b, " %s=%v:%#v", k, v.Kind(), v.RawValue())
			}
			b.WriteByte('\n')
			b.WriteString(render(n.Children().Nodes, depth+1))
			parts = append(parts, b.String())
		}
		if sortSiblings {
			sort.Strings(parts)
		}
		return strings.Join(parts, "")
	}
	return render(doc.Nodes, 0)
}

func lower(t *testing.T, name, src string) string {
	t.Helper()
	out, err := guardfile.Lower(name, []byte(src))
	if err != nil {
		t.Fatalf("Lower(%s): %v", name, err)
	}
	return string(out)
}

const twinKDL = `description "forgejo reads"
wrap ward kdl ops forgejo {
    inherit "base.guardfile.kdl"
    spec forgejo.swagger.v1.json
    base-url "https://forgejo.example/api/v1"
    auth header-token {
        header Authorization
        prefix "token "
        value {
            env "FORGEJO_TOKEN"
            ssm "/forgejo/api-token"
        }
    }
    restrict owner matches "coilyco-*" "kai"
    allow-metacharacters "ref" "path"
    can get "*"
    can create issue state_reason="x" {
        op method="POST" path="/repos/{owner}/{repo}/issues"
        body state="closed" open=#true count=3
        message "filed"
        describe "file an issue"
    }
    cannot delete repo
    never delete "*" {
        reason "destructive"
    }
    override can delete label {
        op "issueDeleteLabel"
    }
}
withhold "playwright" {
    reason "browser is out of scope"
    alternative "use forgejo"
}
reject-empty-argument "create_issue" field="title"
instructions {
    text "read before you write"
    text "say what you filed"
}
`

const twinYAML = `description: forgejo reads
wrap:
  command: [ward, kdl, ops, forgejo]
  inherit: base.guardfile.kdl
  spec: forgejo.swagger.v1.json
  base_url: https://forgejo.example/api/v1
  auth:
    scheme: header-token
    header: Authorization
    prefix: "token "
    value:
      - env: FORGEJO_TOKEN
      - ssm: /forgejo/api-token
  restrict:
    - param: owner
      matches: ["coilyco-*", kai]
  allow_metacharacters: [ref, path]
  can:
    - verb: get
      resource: "*"
    - verb: create
      resource: issue
      props: {state_reason: x}
      op: {method: POST, path: "/repos/{owner}/{repo}/issues"}
      fixed_body: {state: closed, open: true, count: 3}
      message: filed
      describe: file an issue
  cannot:
    - verb: delete
      resource: repo
  never:
    - verb: delete
      resource: "*"
      kdl: 'reason "destructive"'
  override:
    - verb: delete
      resource: label
      op: issueDeleteLabel
withhold:
  - tool: playwright
    reason: browser is out of scope
    alternative: use forgejo
reject_empty_argument:
  - tool: create_issue
    field: title
instructions:
  - read before you write
  - say what you filed
`

const twinTOML = `description = "forgejo reads"
reject_empty_argument = [{ tool = "create_issue", field = "title" }]
instructions = ["read before you write", "say what you filed"]
withhold = [{ tool = "playwright", reason = "browser is out of scope", alternative = "use forgejo" }]

[wrap]
command = ["ward", "kdl", "ops", "forgejo"]
inherit = "base.guardfile.kdl"
spec = "forgejo.swagger.v1.json"
base_url = "https://forgejo.example/api/v1"
allow_metacharacters = ["ref", "path"]

[wrap.auth]
scheme = "header-token"
header = "Authorization"
prefix = "token "
value = [{ env = "FORGEJO_TOKEN" }, { ssm = "/forgejo/api-token" }]

[[wrap.restrict]]
param = "owner"
matches = ["coilyco-*", "kai"]

[[wrap.can]]
verb = "get"
resource = "*"

[[wrap.can]]
verb = "create"
resource = "issue"
props = { state_reason = "x" }
op = { method = "POST", path = "/repos/{owner}/{repo}/issues" }
fixed_body = { state = "closed", open = true, count = 3 }
message = "filed"
describe = "file an issue"

[[wrap.cannot]]
verb = "delete"
resource = "repo"

[[wrap.never]]
verb = "delete"
resource = "*"
kdl = 'reason "destructive"'

[[wrap.override]]
verb = "delete"
resource = "label"
op = "issueDeleteLabel"
`

// The lowered text and the hand-written KDL must parse to the same tree, so a
// YAML or TOML guardfile means what its KDL twin means and no more.
func TestLoweredTwinsParseToTheSameTreeAsKDL(t *testing.T) {
	for name, src := range map[string]string{"twin.yaml": twinYAML, "twin.yml": twinYAML, "twin.toml": twinTOML} {
		sorted := strings.HasSuffix(name, ".toml")
		want, got := dump(t, twinKDL, sorted), dump(t, lower(t, name, src), sorted)
		if got != want {
			t.Errorf("%s lowered to a different tree\n--- want\n%s--- got\n%s", name, want, got)
		}
	}
}

// TOML tables decode unordered, so the key list must restore document order.
func TestTOMLAndYAMLKeepAuthorOrderForGrants(t *testing.T) {
	toml := "[wrap]\ncommand = [\"x\"]\n" +
		"[[wrap.can]]\nverb = \"list\"\nresource = \"b\"\n" +
		"[[wrap.can]]\nverb = \"get\"\nresource = \"a\"\n" +
		"[[wrap.can]]\nverb = \"edit\"\nresource = \"c\"\n"
	yaml := "wrap:\n  command: [x]\n  can:\n" +
		"    - {verb: list, resource: b}\n    - {verb: get, resource: a}\n    - {verb: edit, resource: c}\n"
	for name, src := range map[string]string{"g.toml": toml, "g.yaml": yaml} {
		doc, err := kdl.ParseString(lower(t, name, src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, n := range doc.GetNode("wrap").Children().Nodes {
			got = append(got, n.Arguments()[0].String()+"/"+n.Arguments()[1].String())
		}
		if strings.Join(got, " ") != "list/b get/a edit/c" {
			t.Errorf("%s: grants out of author order: %v", name, got)
		}
	}
}

const inlineKDL = `wrap ward mcp forgejo {
    base-url "https://forgejo.example/api/v1"
    auth query-param {
        param chat_id {
            value env "CHAT"
        }
    }
    can query issue {
        path "/query"
        method GET
        query "state" "page"
        set kind="a_b"
        raw-response
        body {
            field "start" type="integer" required=#true
            field "labels" type="array" items="string"
            object "compositeQuery" required=#true {
                field "end" type="integer"
            }
        }
    }
}
`

const inlineYAML = `wrap:
  command: [ward, mcp, forgejo]
  base_url: https://forgejo.example/api/v1
  auth:
    scheme: query-param
    params:
      - name: chat_id
        value: {env: CHAT}
  can:
    - verb: query
      resource: issue
      path: /query
      method: GET
      query: [state, page]
      set: {kind: a_b}
      raw_response: true
      body:
        - {field: start, type: integer, required: true}
        - {field: labels, type: array, items: string}
        - object: compositeQuery
          required: true
          entries:
            - {field: end, type: integer}
`

func TestInlineDialectLowersAndStillParses(t *testing.T) {
	got := lower(t, "inline.guardfile.yaml", inlineYAML)
	if want := dump(t, inlineKDL, false); dump(t, got, false) != want {
		t.Fatalf("inline YAML lowered to a different tree\n--- want\n%s--- got\n%s", want, dump(t, got, false))
	}
	descs, _, err := opcore.ParseInline([]byte(got))
	if err != nil || len(descs) != 1 {
		t.Fatalf("ParseInline on the lowered text: %d descriptors, err %v", len(descs), err)
	}
}

func TestMappedBodyLowersAndStillParses(t *testing.T) {
	const wantKDL = `wrap ward mcp telegram {
    base-url "https://api.telegram.org"
    auth bearer {
        value env "TOKEN"
    }
    can create message {
        path "/sendMessage"
        body {
            map "commonAnnotations.summary" to="text"
            map "commonLabels.alertname" to="alert_name"
        }
    }
}
`
	const src = `wrap:
  command: [ward, mcp, telegram]
  base_url: https://api.telegram.org
  auth: {scheme: bearer, value: {env: TOKEN}}
  can:
    - verb: create
      resource: message
      path: /sendMessage
      body:
        - {map: commonAnnotations.summary, to: text}
        - {map: commonLabels.alertname, to: alert_name}
`
	got := lower(t, "m.yaml", src)
	if dump(t, got, false) != dump(t, wantKDL, false) {
		t.Fatalf("mapped body lowered to a different tree\n%s", got)
	}
	if _, _, err := opcore.ParseInline([]byte(got)); err != nil {
		t.Fatalf("ParseInline on the lowered text: %v", err)
	}
}

func TestParseFileReadsYAMLAndTOMLLikeKDL(t *testing.T) {
	dir := t.TempDir()
	writeGuardfile(t, dir, "base.guardfile.kdl", `wrap ward kdl ops forgejo {
    spec forgejo.swagger.v1.json
    base-url "https://forgejo.example/api/v1"
    auth none
    can get "*"
}
`)
	yml := writeGuardfile(t, dir, "child.guardfile.yaml", `wrap:
  command: [ward, kdl, ops, forgejo]
  inherit: base.guardfile.kdl
  can:
    - {verb: list, resource: "*"}
`)
	toml := writeGuardfile(t, dir, "child.guardfile.toml", `[wrap]
command = ["ward", "kdl", "ops", "forgejo"]
inherit = "child.guardfile.yaml"

[[wrap.can]]
verb = "create"
resource = "issue"
`)
	gf, err := guardfile.ParseFile(toml)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	got := grantSet(gf)
	for _, want := range []string{"can get *", "can list *", "can create issue"} {
		if !got[want] {
			t.Errorf("grant %q missing after a TOML to YAML to KDL inherit chain: %v", want, got)
		}
	}
	if _, err := guardfile.ParseFile(yml); err != nil {
		t.Fatalf("ParseFile yaml: %v", err)
	}
}

// A string is data. Whatever it holds must come back as one argument.
func TestStringsCannotBreakOutOfTheirNode(t *testing.T) {
	hostile := []string{
		`x" }` + "\n" + `can delete "*" {`,
		"back\\slash and \"quotes\"",
		"line one\nline two",
		"tab\there",
		"#hash /* not a comment */ // nor this",
		"{ braces }",
		"ünïcode ✓",
		"",
		"true",
		"12",
	}
	for _, s := range hostile {
		y := "wrap:\n  command: [x]\n  spec: s.json\n  can:\n    - verb: get\n      resource: r\n      describe: " + yamlQuote(s) + "\n"
		doc, err := kdl.ParseString(lower(t, "h.yaml", y))
		if err != nil {
			t.Fatalf("%q lowered to unparseable KDL: %v", s, err)
		}
		wrap := doc.GetNode("wrap")
		if n := len(wrap.Children().Nodes); n != 2 {
			t.Fatalf("%q broke the wrap body, %d children", s, n)
		}
		can := wrap.Children().GetNode("can")
		if can == nil || len(can.Children().Nodes) != 1 {
			t.Fatalf("%q broke the grant body", s)
		}
		if got := can.Children().Nodes[0].Arguments()[0].String(); got != s {
			t.Errorf("string changed: want %q got %q", s, got)
		}
	}
}

func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestRawKDLMustParseOnItsOwn(t *testing.T) {
	ok := "wrap:\n  command: [x]\n  kdl: 'action a { }'\n"
	if _, err := guardfile.Lower("k.yaml", []byte(ok)); err != nil {
		t.Fatalf("balanced raw KDL rejected: %v", err)
	}
	for name, kdlSrc := range map[string]string{
		"closer":  `} can delete "*" {`,
		"opener":  `action a {`,
		"garbage": `"unterminated`,
	} {
		y := "wrap:\n  command: [x]\n  kdl: " + yamlQuote(kdlSrc) + "\n"
		if _, err := guardfile.Lower("k.yaml", []byte(y)); err == nil {
			t.Errorf("%s: raw KDL that does not parse alone was accepted", name)
		}
	}
}

func TestSchemaFailsClosed(t *testing.T) {
	cases := map[string]struct{ file, src, want string }{
		"unknown top key":     {"a.yaml", "wrap: {command: [x]}\nsurprise: 1\n", `unknown key "surprise"`},
		"unknown wrap key":    {"a.yaml", "wrap: {command: [x], surprise: 1}\n", `unknown key "surprise"`},
		"unknown grant key":   {"a.yaml", "wrap:\n  command: [x]\n  can:\n    - {verb: get, resource: r, surprise: 1}\n", `unknown key "surprise"`},
		"missing command":     {"a.yaml", "wrap: {spec: s.json}\n", `missing required key "command"`},
		"missing verb":        {"a.yaml", "wrap:\n  command: [x]\n  can:\n    - {resource: r}\n", `missing required key "verb"`},
		"wrong type":          {"a.yaml", "wrap:\n  command: [x]\n  spec: [a, b]\n", "want a string"},
		"duplicate yaml key":  {"a.yaml", "wrap: {command: [x]}\nwrap: {command: [y]}\n", "duplicate key"},
		"yaml merge key":      {"a.yaml", "base: &b {command: [x]}\nwrap:\n  <<: *b\n", "merge keys"},
		"two typed kinds":     {"a.yaml", "wrap:\n  command: [x]\n  can:\n    - verb: q\n      resource: r\n      body:\n        - {field: a, object: b}\n", "want one kind"},
		"typed entry no kind": {"a.yaml", "wrap:\n  command: [x]\n  can:\n    - verb: q\n      resource: r\n      body:\n        - {type: string}\n", "names no kind"},
		"unknown toml key":    {"a.toml", "[wrap]\ncommand = [\"x\"]\nsurprise = 1\n", `unknown key "surprise"`},
		"value two providers": {"a.yaml", "wrap:\n  command: [x]\n  auth: {scheme: bearer, value: {env: A, ssm: B}}\n", "exactly one provider"},
	}
	for name, tc := range cases {
		_, err := guardfile.Lower(tc.file, []byte(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

func TestKDLAndUnknownExtensionsPassThroughByteForByte(t *testing.T) {
	const src = "wrap  ward   kdl { }\n// comment kept\n"
	for _, name := range []string{"a.kdl", "a.guardfile.kdl", "a.txt", "a"} {
		got, err := guardfile.Lower(name, []byte(src))
		if err != nil || string(got) != src {
			t.Errorf("%s: want verbatim, got %q err %v", name, got, err)
		}
	}
}

func TestDiscoverySkipsUnrelatedYAMLAndTOML(t *testing.T) {
	skipped := map[string]string{
		"compose.yaml":  "services:\n  web:\n    image: x\n",
		"wrap-flag.yml": "wrap: true\n",
		"wrap-doc.yml":  "wrap:\n  width: 80\n",
		"list.yaml":     "- a\n- b\n",
		"tabs.yaml":     "a: [unterminated\n",
		"cargo.toml":    "[package]\nname = \"x\"\n",
		"empty.yaml":    "",
	}
	for name, src := range skipped {
		if _, err := guardfile.LowerForDiscovery(name, []byte(src)); !errors.Is(err, guardfile.ErrNotGuardfile) {
			t.Errorf("%s: want ErrNotGuardfile, got %v", name, err)
		}
	}
}

func TestDiscoveryFailsOnAMalformedGuardfile(t *testing.T) {
	cases := map[string]string{
		"broken.yaml":  "wrap:\n  command: [x\n",
		"typo.yaml":    "wrap:\n  command: [x]\n  spce: s.json\n",
		"broken.toml":  "[wrap\ncommand = [\"x\"]\n",
		"typo.toml":    "[wrap]\ncommand = [\"x\"]\nspce = \"s\"\n",
		"badgrant.yml": "wrap:\n  command: [x]\n  can:\n    - {resource: r}\n",
	}
	for name, src := range cases {
		out, err := guardfile.LowerForDiscovery(name, []byte(src))
		if err == nil || errors.Is(err, guardfile.ErrNotGuardfile) {
			t.Errorf("%s: a malformed guardfile must fail loudly, got %q err %v", name, out, err)
		}
	}
}

func TestDiscoveryPassesKDLThroughUntouched(t *testing.T) {
	const src = "not even kdl {{{"
	got, err := guardfile.LowerForDiscovery("x.kdl", []byte(src))
	if err != nil || string(got) != src {
		t.Fatalf("KDL discovery must stay the caller's business: %q %v", got, err)
	}
}

func TestIsFormatExtension(t *testing.T) {
	for name, want := range map[string]bool{
		"a.yaml": true, "a.YML": true, "a.toml": true, "a.kdl": false, "a.json": false, "yaml": false,
	} {
		if got := guardfile.IsFormatExtension(name); got != want {
			t.Errorf("IsFormatExtension(%q) = %v, want %v", name, got, want)
		}
	}
}
