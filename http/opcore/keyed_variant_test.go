package opcore_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/coilyco/umbra/http/opcore"
)

// keyedScalarSrc is a `keyed` object whose entry is a bare scalar - the
// `choice.criteria` shape: arbitrary caller-named keys, every value a string.
const keyedScalarSrc = `wrap x {
    auth bearer { value env "T" }
    can create decision {
        path "/v1/systemone"
        body {
            object "criteria" required=true keyed=true {
                entry type="string"
            }
        }
    }
}`

// keyedVariantSrc is the full Jev `questions` shape from umbra#8032: a keyed
// map whose entries are a discriminated union on `type`.
const keyedVariantSrc = `wrap x {
    auth bearer { value env "T" }
    can create decision {
        path "/v1/systemone"
        body {
            object "questions" required=true keyed=true {
                entry {
                    variant on="type" {
                        case "noul" {
                            field "instructions" type="string" required=true
                            object "criteria" {
                                field "true" type="string"
                                field "false" type="string"
                            }
                        }
                        case "choice" {
                            field "instructions" type="string" required=true
                            object "criteria" required=true keyed=true {
                                entry type="string"
                            }
                        }
                        case "score" {
                            field "instructions" type="string" required=true
                            array "criteria" items="string" min-items=2 max-items=10 required=true
                        }
                    }
                }
            }
        }
    }
}`

func TestParseInlineKeyedScalarEntry(t *testing.T) {
	descs, _ := parseInline(t, keyedScalarSrc)
	got := descByLeaf(t, descs, "create")
	if len(got.BodyFlags) != 1 {
		t.Fatalf("body flags = %+v, want exactly one field", got.BodyFlags)
	}
	criteria := got.BodyFlags[0]
	if criteria.Name != "criteria" || criteria.Type != "object" || !criteria.Required {
		t.Fatalf("criteria = %+v", criteria)
	}
	if !criteria.Keyed {
		t.Fatalf("criteria.Keyed = false, want true")
	}
	want := &opcore.Field{Type: "string"}
	if !reflect.DeepEqual(criteria.EntrySchema, want) {
		t.Fatalf("criteria.EntrySchema = %+v, want %+v", criteria.EntrySchema, want)
	}
}

func TestParseInlineKeyedObjectWithVariant(t *testing.T) {
	descs, _ := parseInline(t, keyedVariantSrc)
	got := descByLeaf(t, descs, "create")
	if len(got.BodyFlags) != 1 {
		t.Fatalf("body flags = %+v, want exactly one field", got.BodyFlags)
	}
	questions := got.BodyFlags[0]
	if !questions.Keyed || questions.EntrySchema == nil {
		t.Fatalf("questions = %+v, want Keyed with an EntrySchema", questions)
	}
	entry := questions.EntrySchema
	if entry.Type != "object" || entry.Variant == nil {
		t.Fatalf("questions entry = %+v, want a variant object", entry)
	}
	if entry.Variant.On != "type" {
		t.Fatalf("discriminator = %q, want %q", entry.Variant.On, "type")
	}
	if len(entry.Variant.Cases) != 3 {
		t.Fatalf("cases = %d, want 3 (noul, choice, score)", len(entry.Variant.Cases))
	}

	noul, ok := entry.Variant.Cases["noul"]
	if !ok {
		t.Fatal("missing `noul` case")
	}
	if len(noul) != 2 || noul[0].Name != "instructions" || !noul[0].Required {
		t.Fatalf("noul case fields = %+v", noul)
	}
	if noul[1].Name != "criteria" || noul[1].Keyed {
		t.Fatalf("noul.criteria = %+v, want a fixed two-key object, not keyed", noul[1])
	}

	choice, ok := entry.Variant.Cases["choice"]
	if !ok {
		t.Fatal("missing `choice` case")
	}
	if len(choice) != 2 || choice[1].Name != "criteria" || !choice[1].Keyed || choice[1].EntrySchema == nil {
		t.Fatalf("choice.criteria = %+v, want a required keyed string map", choice)
	}

	score, ok := entry.Variant.Cases["score"]
	if !ok {
		t.Fatal("missing `score` case")
	}
	if len(score) != 2 || score[1].Name != "criteria" || score[1].Type != "array" {
		t.Fatalf("score case fields = %+v", score)
	}
	minItems, maxItems := 2, 10
	if score[1].MinItems == nil || *score[1].MinItems != minItems || score[1].MaxItems == nil || *score[1].MaxItems != maxItems {
		t.Fatalf("score.criteria bounds = %+v, want [%d,%d]", score[1], minItems, maxItems)
	}
}

func TestParseInlineKeyedVariantFailClosedCases(t *testing.T) {
	wrap := func(body string) string {
		return `wrap x {
            auth bearer { value env "T" }
            can create decision { path "/v1/systemone"; body { ` + body + ` } }
        }`
	}
	cases := map[string]string{
		"keyed on a scalar field":                    wrap(`field "x" type="string" keyed=true`),
		"keyed on an array field":                    wrap(`array "x" items="string" keyed=true`),
		"keyed with raw":                             wrap(`object "x" keyed=true raw=true { entry type="string" }`),
		"keyed with no entry":                        wrap(`object "x" keyed=true`),
		"keyed with two children":                    wrap(`object "x" keyed=true { entry type="string"; entry type="integer" }`),
		"keyed with a named field instead of entry":  wrap(`object "x" keyed=true { field "y" type="string" }`),
		"entry with type and block":                  wrap(`object "x" keyed=true { entry type="string" { field "y" type="string" } }`),
		"entry with neither type nor block":          wrap(`object "x" keyed=true { entry }`),
		"entry block with two nodes":                 wrap(`object "x" keyed=true { entry { field "y" type="string"; field "z" type="string" } }`),
		"entry block with a non-variant node":        wrap(`object "x" keyed=true { entry { field "y" type="string" } }`),
		"variant with no on=":                        wrap(`object "x" keyed=true { entry { variant { case "a" { field "y" type="string" } } } }`),
		"variant with empty on=":                     wrap(`object "x" keyed=true { entry { variant on="" { case "a" { field "y" type="string" } } } }`),
		"variant with unknown property":              wrap(`object "x" keyed=true { entry { variant on="type" off="x" { case "a" { field "y" type="string" } } } }`),
		"variant with zero cases":                    wrap(`object "x" keyed=true { entry { variant on="type" { } } }`),
		"variant with a non-case child":              wrap(`object "x" keyed=true { entry { variant on="type" { field "y" type="string" } } }`),
		"variant with a duplicate case":              wrap(`object "x" keyed=true { entry { variant on="type" { case "a" { field "y" type="string" }; case "a" { field "z" type="string" } } } }`),
		"variant case with an empty body":            wrap(`object "x" keyed=true { entry { variant on="type" { case "a" { } } } }`),
		"variant case redeclaring the discriminator": wrap(`object "x" keyed=true { entry { variant on="type" { case "a" { field "type" type="string" } } } }`),
		"min-items on a scalar field":                wrap(`field "x" type="string" min-items=1`),
		"min-items on an object field":               wrap(`object "x" min-items=1 { field "y" type="string" }`),
		"min-items greater than max-items":           wrap(`array "x" items="string" min-items=5 max-items=2`),
		"min-items on a raw array":                   wrap(`array "x" raw=true min-items=1`),
	}
	for name, src := range cases {
		if _, _, err := opcore.ParseInline([]byte(src)); err == nil {
			t.Errorf("%s: expected a fail-closed error, got nil", name)
		}
	}
}

func TestInputSchemaKeyedEmitsAdditionalProperties(t *testing.T) {
	d := opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/v1/systemone",
		BodyFlags: []opcore.Field{
			{
				Name:     "criteria",
				Type:     "object",
				Required: true,
				Keyed:    true,
				EntrySchema: &opcore.Field{
					Type: "string",
				},
			},
		},
	}
	var doc map[string]any
	if err := json.Unmarshal(d.InputSchema().JSONSchema(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	criteria := doc["properties"].(map[string]any)["criteria"].(map[string]any)
	if _, leaked := criteria["properties"]; leaked {
		t.Fatalf("keyed object should not emit a fixed `properties` set: %v", criteria)
	}
	additional, ok := criteria["additionalProperties"].(map[string]any)
	if !ok || additional["type"] != "string" {
		t.Fatalf("additionalProperties = %v, want {type: string}", criteria["additionalProperties"])
	}
}

func TestInputSchemaVariantEmitsOneOfWithConstDiscriminator(t *testing.T) {
	d := opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/v1/systemone",
		BodyFlags: []opcore.Field{
			{
				Name:     "questions",
				Type:     "object",
				Required: true,
				Keyed:    true,
				EntrySchema: &opcore.Field{
					Type: "object",
					Variant: &opcore.Variant{
						On: "type",
						Cases: map[string][]opcore.Field{
							"noul":   {{Name: "instructions", Type: "string", Required: true}},
							"choice": {{Name: "instructions", Type: "string", Required: true}},
						},
					},
				},
			},
		},
	}
	var doc map[string]any
	if err := json.Unmarshal(d.InputSchema().JSONSchema(), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	questions := doc["properties"].(map[string]any)["questions"].(map[string]any)
	entry := questions["additionalProperties"].(map[string]any)
	oneOf, ok := entry["oneOf"].([]any)
	if !ok || len(oneOf) != 2 {
		t.Fatalf("oneOf = %v, want two branches", entry["oneOf"])
	}
	seen := map[string]bool{}
	for _, raw := range oneOf {
		branch := raw.(map[string]any)
		props := branch["properties"].(map[string]any)
		typeConst := props["type"].(map[string]any)["const"].(string)
		seen[typeConst] = true
		required := branch["required"].([]any)
		hasDiscriminator := false
		for _, r := range required {
			if r.(string) == "type" {
				hasDiscriminator = true
			}
		}
		if !hasDiscriminator {
			t.Errorf("branch %q required = %v, want it to include the discriminator", typeConst, required)
		}
	}
	if !seen["noul"] || !seen["choice"] {
		t.Fatalf("oneOf branches = %v, want noul and choice", seen)
	}
}

func TestAssembleBodyKeyedValidatesEveryKey(t *testing.T) {
	d := opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/v1/systemone",
		BodyFlags: []opcore.Field{
			{
				Name:     "questions",
				Type:     "object",
				Required: true,
				Keyed:    true,
				EntrySchema: &opcore.Field{
					Type: "object",
					Fields: []opcore.Field{
						{Name: "instructions", Type: "string", Required: true},
					},
				},
			},
		},
	}
	// Every key's value must satisfy the shared entry shape.
	badBody := map[string]any{
		"questions": map[string]any{
			"is_urgent": map[string]any{}, // missing required "instructions"
		},
	}
	if _, err := opcore.AssembleBody(d, badBody); err == nil {
		t.Fatal("expected a required-field error from a keyed map's entry, got nil")
	}

	goodBody := map[string]any{
		"questions": map[string]any{
			"is_urgent": map[string]any{"instructions": "Is this urgent?"},
		},
	}
	if _, err := opcore.AssembleBody(d, goodBody); err != nil {
		t.Fatalf("valid keyed body: %v", err)
	}
}

func jevVariantDescriptor() opcore.Descriptor {
	return opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/v1/systemone",
		BodyFlags: []opcore.Field{
			{
				Name:     "questions",
				Type:     "object",
				Required: true,
				Keyed:    true,
				EntrySchema: &opcore.Field{
					Type: "object",
					Variant: &opcore.Variant{
						On: "type",
						Cases: map[string][]opcore.Field{
							"noul": {
								{Name: "instructions", Type: "string", Required: true},
							},
							"score": {
								{Name: "instructions", Type: "string", Required: true},
								{Name: "criteria", Type: "array", Items: "string", Required: true},
							},
						},
					},
				},
			},
		},
	}
}

func TestAssembleBodyVariantSelectsCaseByDiscriminator(t *testing.T) {
	d := jevVariantDescriptor()
	body := map[string]any{
		"questions": map[string]any{
			"bug_severity": map[string]any{
				"type":         "score",
				"instructions": "How severe is this?",
				"criteria":     []any{"low", "high"},
			},
		},
	}
	if _, err := opcore.AssembleBody(d, body); err != nil {
		t.Fatalf("valid score variant: %v", err)
	}
}

func TestAssembleBodyVariantFailsClosedOnMissingDiscriminator(t *testing.T) {
	d := jevVariantDescriptor()
	body := map[string]any{
		"questions": map[string]any{
			"bug_severity": map[string]any{
				"instructions": "How severe is this?",
			},
		},
	}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for a missing discriminator, got nil")
	}
}

func TestAssembleBodyVariantFailsClosedOnUnknownDiscriminator(t *testing.T) {
	d := jevVariantDescriptor()
	body := map[string]any{
		"questions": map[string]any{
			"bug_severity": map[string]any{
				"type":         "decide", // not a real case
				"instructions": "How severe is this?",
			},
		},
	}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for an unknown discriminator value, got nil")
	}
}

func TestAssembleBodyVariantFailsClosedOnNonStringDiscriminator(t *testing.T) {
	d := jevVariantDescriptor()
	body := map[string]any{
		"questions": map[string]any{
			"bug_severity": map[string]any{
				"type":         7,
				"instructions": "How severe is this?",
			},
		},
	}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for a non-string discriminator, got nil")
	}
}

func TestAssembleBodyVariantEnforcesCaseOwnRequiredFields(t *testing.T) {
	d := jevVariantDescriptor()
	body := map[string]any{
		"questions": map[string]any{
			"bug_severity": map[string]any{
				"type": "score",
				// missing required "instructions" and "criteria"
			},
		},
	}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for a case missing its own required fields, got nil")
	}
}

// scoreLevelsDescriptor is the `score.criteria` shape alone: an ordered
// array bounded 2-10 items, the rule TypeSafe's own docs state.
func scoreLevelsDescriptor() opcore.Descriptor {
	minItems, maxItems := 2, 10
	return opcore.Descriptor{
		Method: http.MethodPost,
		Path:   "/v1/systemone",
		BodyFlags: []opcore.Field{
			{
				Name:     "criteria",
				Type:     "array",
				Items:    "string",
				Required: true,
				MinItems: &minItems,
				MaxItems: &maxItems,
			},
		},
	}
}

func TestAssembleBodyArrayEnforcesMinItems(t *testing.T) {
	d := scoreLevelsDescriptor()
	body := map[string]any{"criteria": []any{"only one level"}}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for fewer than min-items, got nil")
	}
}

func TestAssembleBodyArrayEnforcesMaxItems(t *testing.T) {
	d := scoreLevelsDescriptor()
	levels := make([]any, 11)
	for i := range levels {
		levels[i] = "level"
	}
	body := map[string]any{"criteria": levels}
	if _, err := opcore.AssembleBody(d, body); err == nil {
		t.Fatal("expected an error for more than max-items, got nil")
	}
}

func TestAssembleBodyArrayWithinBoundsSucceeds(t *testing.T) {
	d := scoreLevelsDescriptor()
	body := map[string]any{"criteria": []any{"low", "medium", "high"}}
	if _, err := opcore.AssembleBody(d, body); err != nil {
		t.Fatalf("array within bounds: %v", err)
	}
}
