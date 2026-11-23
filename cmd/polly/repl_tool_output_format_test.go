package main

import (
	"context"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

var goFileRead = messages.ChatMessageToolCall{Name: "read_file", Arguments: `{"path":"main.go"}`}

// halfAFormatting is half of what highlighting one goFileBody allocates.
// That is tens of thousands of allocations, so work that stays under half
// did not highlight a body, and work over half highlighted at least one.
func halfAFormatting() float64 {
	return testing.AllocsPerRun(1, func() { formatInlineToolOutput(goFileBody, "Go") }) / 2
}

// An output is formatted when a row first shows it, not when it is built:
// replay builds one for every call, and most are never opened. Whoever shows
// one, on whatever goroutine, gets the text that formatting it up front gave.
func TestToolOutputFormatsWhenFirstShown(t *testing.T) {
	history := readFileHistory(1, 20, goFileBody)
	if replay, half := testing.AllocsPerRun(1, func() { newReplModel().hydrateHistory(history, "ctx") }), halfAFormatting(); replay >= half {
		t.Fatalf("replaying 20 reads allocated %.0f times, over half of what formatting one takes (%.0f): replay formatted rows nobody opened", replay, half)
	}
	output := newInlineToolOutput(goFileRead, messages.ChatMessage{Role: messages.MessageRoleTool, Content: goFileBody})
	want := formatInlineToolOutput(goFileBody, "Go")
	var rows sync.WaitGroup
	for range 4 {
		rows.Go(func() {
			if output.text() != want || output.empty() {
				t.Error("a shown page differs from its formatted body")
			}
		})
	}
	rows.Wait()
	if again := testing.AllocsPerRun(100, func() { output.text(); output.empty() }); again != 0 {
		t.Fatalf("showing a page again allocated %.0f times: it was formatted more than once", again)
	}
}

// A page read from the artifact store is formatted by the read, on its
// worker: the event loop that shows the page only lays it out.
func TestArtifactPageIsFormattedByItsRead(t *testing.T) {
	store := testArtifactStore(t)
	ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte(goFileBody)})
	if err != nil {
		t.Fatal(err)
	}
	source := newInlineToolOutput(goFileRead, messages.ChatMessage{Role: messages.MessageRoleTool, Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
	var page *inlineToolOutput
	if reading, half := testing.AllocsPerRun(1, func() { page = readInlineToolOutput(context.Background(), store, source, 0) }), halfAFormatting(); reading <= half {
		t.Fatalf("reading the page allocated %.0f times, under half of what formatting it takes (%.0f): the read left formatting to whoever shows it", reading, half)
	}
	if want := formatInlineToolOutput(goFileBody, "Go"); !page.loaded || page.text() != want {
		t.Fatalf("read page shows %q, want %q", page.text(), want)
	}
}

// A page with no text says so on the label row and has no body.
func TestBlankToolOutputPageSaysNoOutput(t *testing.T) {
	for _, page := range []struct {
		body, label string
	}{
		{" \n\t\r\n", "no output"},
		{"\x1b[0m\n", "no output"},
		{"", "no output"},
		{"done\n", "output"},
	} {
		row := toolDisclosureRow{settled: true, outputExpanded: true, output: newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: page.body})}
		label, body, _, _ := row.inlineOutputSection(80)
		if plainStyledText(label) != page.label || (body == "") != (page.label == "no output") {
			t.Fatalf("page %q: label %q with body %q, want label %q", page.body, plainStyledText(label), body, page.label)
		}
	}
}
