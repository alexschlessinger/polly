package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func testSchema() *llm.Schema {
	return &llm.Schema{Raw: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"name", "kind", "tags"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"kind": map[string]any{"type": "string", "enum": []any{"file", "dir"}},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}, Strict: true}
}

func TestWriteStructuredPrintsValidOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := writeStructured(&stdout, &stderr, `{"name":"a","kind":"file","tags":["x"]}`, testSchema())
	if err != nil {
		t.Fatalf("writeStructured: %v", err)
	}
	if !strings.Contains(stdout.String(), `"name": "a"`) || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestWriteStructuredFailsOnMalformedJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := writeStructured(&stdout, &stderr, `{"name": "a",`, testSchema())
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("err = %v, want invalid JSON error", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("malformed output reached stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `{"name": "a",`) {
		t.Fatalf("raw reply missing from stderr: %q", stderr.String())
	}
}

func TestWriteStructuredFailsOnEmptyOutput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeStructured(&stdout, &stderr, "  \n", testSchema()); err == nil {
		t.Fatal("empty output was accepted")
	}
}

func TestWriteStructuredEnforcesNestedConstraints(t *testing.T) {
	cases := map[string]string{
		"enum":                 `{"name":"a","kind":"symlink","tags":[]}`,
		"nested item type":     `{"name":"a","kind":"file","tags":[1]}`,
		"additionalProperties": `{"name":"a","kind":"file","tags":[],"extra":true}`,
		"required":             `{"name":"a","kind":"file"}`,
		"top-level type":       `[]`,
	}
	for name, content := range cases {
		var stdout, stderr bytes.Buffer
		err := writeStructured(&stdout, &stderr, content, testSchema())
		if err == nil || !strings.Contains(err.Error(), "does not match the schema") {
			t.Errorf("%s: err = %v, want schema mismatch", name, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%s: off-schema output reached stdout: %q", name, stdout.String())
		}
	}
}

func TestWriteStructuredWithoutSchemaStillRequiresJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := writeStructured(&stdout, &stderr, `{"any":1}`, nil); err != nil {
		t.Fatalf("writeStructured without schema: %v", err)
	}
	if err := writeStructured(&stdout, &stderr, `not json`, nil); err == nil {
		t.Fatal("non-JSON accepted without a schema")
	}
}
