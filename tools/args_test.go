package tools

import (
	"testing"
)

func TestArgs_String(t *testing.T) {
	a := Args{"name": "alice", "count": 42}
	if got := a.String("name"); got != "alice" {
		t.Errorf("String(name) = %q, want alice", got)
	}
	if got := a.String("missing"); got != "" {
		t.Errorf("String(missing) = %q, want empty", got)
	}
	if got := a.String("count"); got != "" {
		t.Errorf("String(count) = %q, want empty (wrong type)", got)
	}
}

func TestArgs_Int(t *testing.T) {
	a := Args{
		"float":   float64(42),
		"int":     int(7),
		"int64":   int64(99),
		"str":     "nope",
		"zero":    float64(0),
		"neg":     float64(-5),
	}
	if got := a.Int("float", 0); got != 42 {
		t.Errorf("Int(float) = %d, want 42", got)
	}
	if got := a.Int("int", 0); got != 7 {
		t.Errorf("Int(int) = %d, want 7", got)
	}
	if got := a.Int("int64", 0); got != 99 {
		t.Errorf("Int(int64) = %d, want 99", got)
	}
	if got := a.Int("str", 10); got != 10 {
		t.Errorf("Int(str) = %d, want 10 (default)", got)
	}
	if got := a.Int("missing", 10); got != 10 {
		t.Errorf("Int(missing) = %d, want 10 (default)", got)
	}
	if got := a.Int("zero", 10); got != 0 {
		t.Errorf("Int(zero) = %d, want 0", got)
	}
	if got := a.Int("neg", 0); got != -5 {
		t.Errorf("Int(neg) = %d, want -5", got)
	}
}

func TestArgs_Float(t *testing.T) {
	a := Args{"f": float64(3.14), "i": int(7), "i64": int64(99)}
	if got := a.Float("f", 0); got != 3.14 {
		t.Errorf("Float(f) = %f, want 3.14", got)
	}
	if got := a.Float("i", 0); got != 7.0 {
		t.Errorf("Float(i) = %f, want 7.0", got)
	}
	if got := a.Float("i64", 0); got != 99.0 {
		t.Errorf("Float(i64) = %f, want 99.0", got)
	}
	if got := a.Float("missing", 1.5); got != 1.5 {
		t.Errorf("Float(missing) = %f, want 1.5", got)
	}
}

func TestArgs_Bool(t *testing.T) {
	a := Args{"yes": true, "no": false, "str": "true"}
	if !a.Bool("yes") {
		t.Error("Bool(yes) = false, want true")
	}
	if a.Bool("no") {
		t.Error("Bool(no) = true, want false")
	}
	if a.Bool("str") {
		t.Error("Bool(str) = true, want false (wrong type)")
	}
	if a.Bool("missing") {
		t.Error("Bool(missing) = true, want false")
	}
}

func TestArgs_StringSlice(t *testing.T) {
	t.Run("from []any", func(t *testing.T) {
		a := Args{"ids": []any{"a", "b", "c"}}
		got := a.StringSlice("ids")
		want := []string{"a", "b", "c"}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("from []string", func(t *testing.T) {
		a := Args{"ids": []string{"x", "y"}}
		got := a.StringSlice("ids")
		if len(got) != 2 || got[0] != "x" || got[1] != "y" {
			t.Errorf("got %v, want [x y]", got)
		}
	})

	t.Run("deduplicates", func(t *testing.T) {
		a := Args{"ids": []any{"a", "b", "a", "c", "b"}}
		got := a.StringSlice("ids")
		want := []string{"a", "b", "c"}
		if len(got) != len(want) {
			t.Fatalf("len = %d, want %d: %v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("skips empty strings", func(t *testing.T) {
		a := Args{"ids": []any{"a", "", "b"}}
		got := a.StringSlice("ids")
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("got %v, want [a b]", got)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		a := Args{}
		if got := a.StringSlice("ids"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("wrong type", func(t *testing.T) {
		a := Args{"ids": "not a slice"}
		if got := a.StringSlice("ids"); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("mixed types in []any", func(t *testing.T) {
		a := Args{"ids": []any{"a", 42, "b", true}}
		got := a.StringSlice("ids")
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("got %v, want [a b]", got)
		}
	})
}

func TestArgs_NilMap(t *testing.T) {
	var a Args
	if got := a.String("x"); got != "" {
		t.Errorf("String on nil = %q", got)
	}
	if got := a.Int("x", 5); got != 5 {
		t.Errorf("Int on nil = %d", got)
	}
	if got := a.Bool("x"); got {
		t.Error("Bool on nil = true")
	}
	if got := a.StringSlice("x"); got != nil {
		t.Errorf("StringSlice on nil = %v", got)
	}
}
