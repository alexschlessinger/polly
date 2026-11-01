package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestInlineOutputFormattingBudgetKeepsCompleteRawJSON(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"compact array", "[" + strings.Repeat("0,", 1000) + "0]"},
		{"nested array", strings.Repeat("[", 700) + "0" + strings.Repeat("]", 700)},
		{"escaped newlines", strconv.Quote(strings.Repeat("line\n", 1001))},
		{"escaped returns", strconv.Quote(strings.Repeat("line\r", 1001))},
		{"escaped tabs", strconv.Quote("prefix" + strings.Repeat("\t", 70000) + "tail")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.body)) || len(tc.body) > inlineOutputBytes {
				t.Fatal("fixture must be valid JSON within one raw page")
			}
			o := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: tc.body})
			text := plainStyledText(o.text)
			if len(text) > inlineOutputBytes || strings.Count(text, "\n")+1 > inlineOutputLines {
				t.Fatalf("formatting escaped the page budget: raw=%d display=%d bytes/%d lines", len(tc.body), len(text), strings.Count(text, "\n")+1)
			}
			if text != tc.body || o.more || o.end != int64(len(tc.body)) {
				t.Fatalf("oversized formatting must fall back to complete raw JSON, without truncation or a fake continuation: display bytes=%d end=%d more=%v", len(text), o.end, o.more)
			}
		})
	}
}

func TestInlineJSONIndentBudgetPreflight(t *testing.T) {
	for _, body := range []string{
		`{"quoted":` + strconv.Quote(strings.Repeat(`\"[{,}:]`, 1000)) + `,"empty":[]}`,
		"[" + strings.Repeat("0,", inlineOutputLines-3) + "0]",
		strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100),
	} {
		if !inlineJSONIndentFits(body) {
			t.Fatal("safe JSON formatting was unnecessarily rejected")
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(body), "", "  "); err != nil {
			t.Fatal(err)
		}
		o := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: body})
		if got := plainStyledText(o.text); got != pretty.String() || len(got) > inlineOutputBytes || strings.Count(got, "\n")+1 > inlineOutputLines {
			t.Fatal("preflight changed safe formatting or undercounted its budget")
		}
	}
	for _, body := range []string{
		"[" + strings.Repeat("0,", inlineOutputLines-2) + "0]", // One formatted line over.
		strings.Repeat("[", 700) + "0" + strings.Repeat("]", 700),
		`{"s":"` + strings.Repeat("x", inlineOutputBytes-8) + `"}`, // Indentation grows past the byte cap.
	} {
		if inlineJSONIndentFits(body) {
			t.Fatal("unsafe indentation was allowed before allocation")
		}
		o := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: body})
		if plainStyledText(o.text) != body || o.more {
			t.Fatal("unsafe indentation did not preserve the complete raw page")
		}
	}
}

func TestInlineOutputCarriageReturnPages(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("line\r", 1001),
		strings.Repeat("one\rtwo\r\nthree\n", 401),
		strings.Repeat("line\r", inlineOutputLines-1) + "split\r\nlast",
	} {
		for _, artifact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/artifact=%v", len(body), artifact), func(t *testing.T) {
				o := newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: body})
				var store artifacts.Store
				if artifact {
					store = testArtifactStore(t)
					ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte(body)})
					if err != nil {
						t.Fatal(err)
					}
					o = newInlineToolOutput(messages.ChatMessageToolCall{}, messages.ChatMessage{Content: "preview", Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
					o = readInlineToolOutput(context.Background(), store, o, 0)
				}
				var captured strings.Builder
				pages := 0
				for {
					pages++
					text := plainStyledText(o.text)
					if o.notice != "" || o.offset != int64(captured.Len()) || o.end <= o.offset || strings.Count(text, "\n")+1 > inlineOutputLines {
						t.Fatalf("CR page has invalid progress or budget: offset=%d end=%d lines=%d notice=%q", o.offset, o.end, strings.Count(text, "\n")+1, o.notice)
					}
					part := body[o.offset:o.end]
					if o.more && strings.HasSuffix(part, "\r") && body[o.end] == '\n' {
						t.Fatal("page split a CRLF pair")
					}
					if text != strings.TrimRight(cleanInlineToolOutput(part), "\n") {
						t.Fatal("page lost recorded CR-delimited text")
					}
					captured.WriteString(part)
					if !o.more {
						break
					}
					if artifact {
						o = readInlineToolOutput(context.Background(), store, o, 1)
					} else {
						o = o.pageRequest(1)
						o.setPage(body[o.offset:])
					}
				}
				if pages < 2 || captured.String() != body {
					t.Fatal("CR paging stranded recorded content")
				}
			})
		}
	}
}
