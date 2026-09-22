package opcore

import (
	"encoding/json"
	"reflect"
	"testing"
)

// decode is a small json.Unmarshal-into-any helper, so a fixture reads like
// the wire bytes it stands in for.
func decode(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return v
}

func TestPruneReturnFieldsNoDeclarationPassesThrough(t *testing.T) {
	v := decode(t, `{"id":1,"secret":"s3cret"}`)
	got := pruneReturnFields(v, nil)
	if !reflect.DeepEqual(got, v) {
		t.Errorf("pruned = %#v, want unchanged %#v", got, v)
	}
}

func TestPruneReturnFieldsDropsUndeclaredKeys(t *testing.T) {
	v := decode(t, `{"id":1,"name":"aos","internal_token":"s3cret"}`)
	got := pruneReturnFields(v, []Field{{Name: "id", Type: "integer"}, {Name: "name", Type: "string"}})
	want := decode(t, `{"id":1,"name":"aos"}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsMissingDeclaredKeyIsSkipped(t *testing.T) {
	v := decode(t, `{"id":1}`)
	got := pruneReturnFields(v, []Field{{Name: "id", Type: "integer"}, {Name: "name", Type: "string"}})
	want := decode(t, `{"id":1}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsNonObjectRootPassesThrough(t *testing.T) {
	v := decode(t, `[1,2,3]`)
	got := pruneReturnFields(v, []Field{{Name: "id", Type: "integer"}})
	if !reflect.DeepEqual(got, v) {
		t.Errorf("pruned = %#v, want unchanged %#v", got, v)
	}
}

func TestPruneReturnFieldsNestedObject(t *testing.T) {
	v := decode(t, `{"id":1,"owner":{"login":"kai","email":"kai@example.com"}}`)
	fields := []Field{
		{Name: "id", Type: "integer"},
		{Name: "owner", Type: "object", Fields: []Field{{Name: "login", Type: "string"}}},
	}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"id":1,"owner":{"login":"kai"}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsArrayOfObjects(t *testing.T) {
	v := decode(t, `{"topics":[{"name":"go","internal_id":9},{"name":"kdl","internal_id":10}]}`)
	fields := []Field{
		{Name: "topics", Type: "array", Item: &Field{Type: "object", Fields: []Field{{Name: "name", Type: "string"}}}},
	}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"topics":[{"name":"go"},{"name":"kdl"}]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsArrayWithNoItemPassesThrough(t *testing.T) {
	v := decode(t, `{"scores":[1,2,3]}`)
	fields := []Field{{Name: "scores", Type: "array"}}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"scores":[1,2,3]}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsRawFieldKeepsValueWhole(t *testing.T) {
	v := decode(t, `{"metadata":{"anything":"at all","nested":{"too":true}}}`)
	fields := []Field{{Name: "metadata", Type: "object", Raw: true}}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"metadata":{"anything":"at all","nested":{"too":true}}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsKeyedObjectKeepsEveryKey(t *testing.T) {
	v := decode(t, `{"scores":{"alice":{"value":9,"note":"internal"},"bob":{"value":7,"note":"internal"}}}`)
	fields := []Field{{
		Name: "scores", Type: "object", Keyed: true,
		EntrySchema: &Field{Type: "object", Fields: []Field{{Name: "value", Type: "integer"}}},
	}}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"scores":{"alice":{"value":9},"bob":{"value":7}}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsVariantSelectsCaseAndKeepsDiscriminator(t *testing.T) {
	v := decode(t, `{"question":{"type":"choice","instructions":"pick one","internal_weight":0.5}}`)
	fields := []Field{{
		Name: "question", Type: "object",
		Variant: &Variant{
			On: "type",
			Cases: map[string][]Field{
				"choice": {{Name: "instructions", Type: "string"}},
				"score":  {{Name: "criteria", Type: "array"}},
			},
		},
	}}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"question":{"type":"choice","instructions":"pick one"}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}

func TestPruneReturnFieldsVariantUnknownDiscriminatorPassesThrough(t *testing.T) {
	v := decode(t, `{"question":{"type":"unknown_case","x":1}}`)
	fields := []Field{{
		Name: "question", Type: "object",
		Variant: &Variant{On: "type", Cases: map[string][]Field{"choice": {{Name: "instructions", Type: "string"}}}},
	}}
	got := pruneReturnFields(v, fields)
	want := decode(t, `{"question":{"type":"unknown_case","x":1}}`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruned = %#v, want %#v", got, want)
	}
}
