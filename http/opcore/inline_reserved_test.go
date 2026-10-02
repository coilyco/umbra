package opcore_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
)

// The Exa search body: a required `query` beside optional fields, which the CLI's
// reserved flag names refuse and a consumer with no CLI flags must be able to state.
const reservedNameSrc = `wrap ward mcp exa {
    auth bearer { value env "T" }
    can search web {
        method "POST"
        path "/search"
        body {
            field "query" type="string" required=true
            field "numResults" type="integer"
            object "contents" raw=true
        }
    }
}`

func TestParseInlineStillRefusesAReservedBodyNameByDefault(t *testing.T) {
	if _, _, err := opcore.ParseInline([]byte(reservedNameSrc)); err == nil {
		t.Fatal("the default grammar keeps the CLI's reserved flag names off inputs")
	}
}

func TestParseInlineWithAllowReservedNamesStatesTheBodyField(t *testing.T) {
	descs, _, _, err := opcore.ParseInlineWithOptions([]byte(reservedNameSrc), opcore.InlineOptions{AllowReservedNames: true})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range descs[0].BodyFlags {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"query", "numResults", "contents"}) {
		t.Fatalf("body fields = %v", names)
	}
}

func TestAllowReservedNamesStillRefusesDuplicateInputs(t *testing.T) {
	_, _, _, err := opcore.ParseInlineWithOptions([]byte(`wrap x {
        auth bearer { value env "T" }
        can create search { path "/s"; query "query"; body "query" }
    }`), opcore.InlineOptions{AllowReservedNames: true})
	if err == nil {
		t.Fatal("a query and a body input both named `query` still collide")
	}
}

func TestReservedNameBodyFieldReachesTheWireUnrenamed(t *testing.T) {
	descs, _, _, err := opcore.ParseInlineWithOptions([]byte(reservedNameSrc), opcore.InlineOptions{AllowReservedNames: true})
	if err != nil {
		t.Fatal(err)
	}
	op, got := bodyEcho(t, descs[0])
	if _, err := op.Execute(context.Background(), opcore.Args{Body: map[string]any{
		"query": "exa vs tavily", "numResults": float64(3), "contents": map[string]any{"text": true},
	}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"query": "exa vs tavily", "numResults": float64(3), "contents": map[string]any{"text": true}}
	if !reflect.DeepEqual(*got, want) {
		t.Fatalf("wire body = %v, want %v", *got, want)
	}
}
