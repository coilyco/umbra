package opcore_test

import (
	"testing"

	"github.com/coilyco/umbra/http/opcore"
)

// COI-2083: each shape reached Jev as a 422 because the validator skipped
// any value that was not the declared JSON type.
func TestAssembleBodyRefusesWrongTypedShapes(t *testing.T) {
	d := jevVariantDescriptor()
	cases := map[string]any{
		"questions as bare string":         "Is it going to rain?",
		"questions as JSON-encoded string": `[{"question": "Is it going to rain?"}]`,
		"questions as array":               []any{map[string]any{"question": "Is it going to rain?"}},
		"entry as bare string":             map[string]any{"question": "Is it going to rain?"},
		"entry as array":                   map[string]any{"q": []any{"x"}},
		"score criteria as string":         map[string]any{"q": map[string]any{"type": "score", "instructions": "x", "criteria": "low,high"}},
	}
	for name, questions := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := opcore.AssembleBody(d, map[string]any{"questions": questions}); err == nil {
				t.Fatalf("questions = %#v reached the wire, want a local refusal", questions)
			}
		})
	}
}
