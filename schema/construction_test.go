package schema

import "testing"

func TestSchemaConstructionReportsInvalidDefinitions(t *testing.T) {
	for _, input := range []string{"", `{"type":`, "null", "[]", `{"type":"made_up"}`, `{"type":"string","type":"number"}`, `{"$ref":"#/$defs/missing"}`} {
		s, err := SchemaFromJSON(input)
		if err == nil || s != nil {
			t.Errorf("SchemaFromJSON(%q)=(%+v,%v)", input, s, err)
		}
	}
	for _, input := range []any{nil, make(chan int), func() {}} {
		s, err := SchemaFor(input)
		if err == nil || s != nil {
			t.Errorf("SchemaFor(%T)=(%+v,%v)", input, s, err)
		}
	}
	s, err := SchemaFromJSON(`{}`)
	if err != nil || s == nil {
		t.Fatalf("explicit unconstrained schema rejected: %v", err)
	}
}

func TestMustSchemaPanicsOnInvalidDefinition(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("invalid static schema did not panic")
		}
	}()
	MustSchemaFromJSON(`{"type":`)
}
