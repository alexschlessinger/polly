package coordindex

import (
	"errors"
	"testing"
)

func TestNotificationRevisionScopeNormalization(t *testing.T) {
	for _, index := range []Index{SemanticNews, Display} {
		got, key, err := Normalize(index, "")
		if err != nil || got != index || key != "" {
			t.Fatalf("empty-key scope %s: %s %q %v", index, got, key, err)
		}
		if _, _, err := Normalize(index, Key("")); !errors.Is(err, ErrSelector) {
			t.Fatalf("accepted nonempty %s key: %v", index, err)
		}
	}
	for _, key := range []string{Key("member", "m"), Key("workflow", "m"), Key("status", "parent"), Key("launch", "e"), Key("", ""), Key("\x00\xff", "a/b")} {
		got, normalized, err := Normalize(DisplayFact, key)
		if err != nil || got != DisplayFact || normalized != key {
			t.Fatalf("fact key %q: %s %q %v", key, got, normalized, err)
		}
	}
	for _, key := range []string{"", "!", Key("member"), Key("member", "m", "extra")} {
		if _, _, err := Normalize(DisplayFact, key); !errors.Is(err, ErrSelector) {
			t.Fatalf("accepted malformed fact key %q: %v", key, err)
		}
	}
	for _, key := range []string{"", "reader", "reader/", "\x00\xff"} {
		got, normalized, err := Normalize(SemanticMail, key)
		if err != nil || got != SemanticMail || normalized != key {
			t.Fatalf("mail key %q: %s %q %v", key, got, normalized, err)
		}
	}
}
