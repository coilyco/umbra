package opcore

// pruneReturnFields narrows decoded to fields, dropping every other key.
// Nil or empty fields returns decoded unchanged. See docs/opcore-returns.md.
func pruneReturnFields(decoded any, fields []Field) any {
	if len(fields) == 0 {
		return decoded
	}
	return pruneValue(decoded, fields)
}

// pruneValue prunes an object root against fields; any other value (an array
// or scalar response root) has no keys to drop and passes through whole.
func pruneValue(v any, fields []Field) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	return pruneObject(obj, fields)
}

// pruneObject keeps only the keys fields names, recursing into each kept
// value. A key fields does not name is dropped even when present.
func pruneObject(obj map[string]any, fields []Field) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, present := obj[f.Name]; present {
			out[f.Name] = pruneField(v, f)
		}
	}
	return out
}

// pruneField applies one field's declared shape to its present value. A raw
// field, or one whose type is not object/array, keeps its value whole.
func pruneField(v any, f Field) any {
	if f.Raw {
		return v
	}
	switch f.Type {
	case "object":
		return pruneObjectField(v, f)
	case "array":
		return pruneArrayField(v, f)
	default:
		return v
	}
}

// pruneObjectField dispatches a nested object to its keyed, variant, or
// fixed shape, mirroring validateObjectBodyValue's cases in reverse.
func pruneObjectField(v any, f Field) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return v
	}
	switch {
	case f.Variant != nil:
		return pruneVariantField(obj, *f.Variant)
	case f.Keyed:
		return pruneKeyedField(obj, f)
	case len(f.Fields) > 0:
		return pruneObject(obj, f.Fields)
	default:
		return obj
	}
}

// pruneKeyedField prunes every caller-chosen key's value against
// EntrySchema, keeping the key set itself whole - it is the caller's.
func pruneKeyedField(obj map[string]any, f Field) map[string]any {
	if f.EntrySchema == nil {
		return obj
	}
	out := make(map[string]any, len(obj))
	for key, v := range obj {
		out[key] = pruneField(v, *f.EntrySchema)
	}
	return out
}

// pruneVariantField selects a case by its discriminator and prunes against
// that case's fields. Unreadable or unrecognized stays untouched.
func pruneVariantField(obj map[string]any, variant Variant) map[string]any {
	discRaw, present := obj[variant.On]
	disc, ok := discRaw.(string)
	if !present || !ok {
		return obj
	}
	fields, known := variant.Cases[disc]
	if !known {
		return obj
	}
	pruned := pruneObject(obj, fields)
	pruned[variant.On] = disc
	return pruned
}

// pruneArrayField prunes every element against Item. No declared Item, or a
// non-array value, passes through whole.
func pruneArrayField(v any, f Field) any {
	items, ok := v.([]any)
	if !ok || f.Item == nil {
		return v
	}
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = pruneField(item, *f.Item)
	}
	return out
}
