package coordindex

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestPublicationExactEquivalenceAndEligibility(t *testing.T) {
	base := Publication{ID: "a", Run: "r", Key: "key", Text: "  claim\n", Posted: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	same := base
	same.ID = "b"
	same.Text = "claim"
	same.Scope = "run"
	if !SamePublicationFact(base, same, "commit", "commit") {
		t.Fatal("trimmed text/default scope")
	}
	for _, change := range []func(*Publication){func(p *Publication) { p.Scope = "workspace" }, func(p *Publication) { p.Run = "other" }, func(p *Publication) { p.Key = "other" }, func(p *Publication) { p.Value = "claim" }, func(p *Publication) { p.Text = "different" }} {
		other := same
		change(&other)
		if SamePublicationFact(base, other, "commit", "commit") {
			t.Fatalf("coalesced %+v", other)
		}
	}
	if SamePublicationFact(base, same, "commit", "different") {
		t.Fatal("snapshot commits lost")
	}
	base.Value = "value"
	same.Value = "value"
	same.Text = "unrelated evidence"
	if !SamePublicationFact(base, same, "commit", "commit") {
		t.Fatal("canonical value must ignore evidence wording")
	}
	base.Kind = "host"
	same.Scope = "machine"
	same.Run = "other"
	if !SamePublicationFact(base, same, "commit", "commit") {
		t.Fatal("machine scope must ignore original run")
	}
	reader := PublicationReaderState{ID: "reader", Run: "r", Posted: base.Posted.Add(time.Hour)}
	base.DuplicateOf = "old"
	if !PublicationPendingFor(base, true, true, reader) {
		t.Fatal("promoted confirmation before mark lost")
	}
	if PublicationPendingFor(base, true, false, reader) {
		t.Fatal("coalesced duplicate delivered")
	}
	base.AdmittedTo = []string{"reader"}
	if PublicationPendingFor(base, true, true, reader) {
		t.Fatal("exact receipt ignored")
	}
	base.DuplicateOf = ""
	reader.Posted = time.Time{}
	if !PublicationPendingFor(base, true, false, reader) {
		t.Fatal("historical nonduplicate event lost")
	}
	if PublicationPendingFor(base, false, true, reader) {
		t.Fatal("explicit withdrawal ignored")
	}
	reader.ID = base.Author
	if PublicationPendingFor(base, true, true, reader) {
		t.Fatal("author not excluded")
	}
}
func TestAdmissionNewsProjection(t *testing.T) {
	pairs := [][3]string{
		{"member", `{"execution":"e","label":"one"}`, `{"execution":"e","label":"two"}`},
		{"execution", `{"status":"queued","generation":2}`, `{"status":"running","generation":2,"started":"2026-01-01T00:00:00Z"}`},
		{"workflow", `{"status":"running","acknowledged":false}`, `{"status":"running","steps":[],"input":"large"}`},
	}
	for _, p := range pairs {
		a, err := NewsProjection(p[0], json.RawMessage(p[1]))
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewsProjection(p[0], json.RawMessage(p[2]))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("non-news %s: %s -> %s", p[0], a, b)
		}
	}
	a, err := NewsProjection("execution", json.RawMessage(`{"generation":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewsProjection("execution", json.RawMessage(`{"generation":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("integer news rounded through float64")
	}
	if _, err := NewsProjection("task", json.RawMessage(`{"deferral":{"workflow":"w","generation":1}}`)); err != nil {
		t.Fatal(err)
	}
	if p, err := NewsProjection("custom", json.RawMessage(`[1,2,3]`)); err != nil || p != nil {
		t.Fatalf("unknown kind: %s %v", p, err)
	}
}
