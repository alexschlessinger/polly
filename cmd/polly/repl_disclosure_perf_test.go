package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// BenchmarkREPLDisclosureCycle measures three Ctrl-O events per operation:
// folded -> open -> detailed -> folded. toggle-render paints after EVERY event;
// toggle measures only event handling (including any held-viewport re-layout).
// Both start with a warmed, folded transcript. Fixtures and the warm-up cycle
// are outside the timer. Rendering uses the real REPL with a headless screen,
// excluding terminal I/O, native images, providers, and artifact disk reads.
//
// groups has one completed thought and one completed tool per prose-delimited
// group. dense has one thought and a single batch of tools, exercising layout
// of many output links within one activity block and catching repeated-prefix
// layout regressions.
// Each tool has twelve distinct, multiline output rows already loaded.
//
// A bounded baseline run (ns/op and B/op are per THREE-event cycle):
//
//	go test ./cmd/polly -run '^$' -bench '^BenchmarkREPLDisclosureCycle$' -benchtime=1x -benchmem -count=1
//
// Select one leaf for profiling; keep the binary and profiles outside the tree:
//
//	go test ./cmd/polly -run '^$' -bench '^BenchmarkREPLDisclosureCycle/dense/250/held/toggle-render$' -benchtime=3x -cpuprofile "$TMPDIR/disclosure.cpu" -memprofile "$TMPDIR/disclosure.mem" -o "$TMPDIR/disclosure.test"
func BenchmarkREPLDisclosureCycle(b *testing.B) {
	for _, shape := range []struct {
		name  string
		sizes []int
	}{
		{"groups", []int{100, 1000}},
		{"dense", []int{100, 250, 1000}},
	} {
		b.Run(shape.name, func(b *testing.B) {
			for _, count := range shape.sizes {
				b.Run(fmt.Sprint(count), func(b *testing.B) {
					for _, viewport := range []string{"following", "held"} {
						b.Run(viewport, func(b *testing.B) {
							for _, mode := range []string{"toggle", "toggle-render"} {
								b.Run(mode, func(b *testing.B) {
									r := newDisclosureBenchmarkREPL(b, count, shape.name == "dense", viewport == "held")
									event := ui.Event{Type: ui.KeyboardEvent, ID: "<C-o>"}
									foldedRows := len(r.transcriptW.Rows)
									openRows, detailedRows := 0, 0
									anchor := r.model.scrollAnchor
									for _, want := range []disclosureMode{disclosuresOpen, disclosuresDetailed, disclosuresFolded} {
										r.handleEvent(event)
										r.render()
										if r.model.disclosures != want {
											b.Fatalf("Ctrl-O level = %d, want %d", r.model.disclosures, want)
										}
										switch want {
										case disclosuresOpen:
											openRows = len(r.transcriptW.Rows)
										case disclosuresDetailed:
											detailedRows = len(r.transcriptW.Rows)
										}
									}
									if openRows <= foldedRows || detailedRows <= openRows || len(r.transcriptW.Rows) != foldedRows {
										b.Fatalf("cycle row counts: folded=%d open=%d detailed=%d final=%d", foldedRows, openRows, detailedRows, len(r.transcriptW.Rows))
									}
									if viewport == "held" && (r.model.followBottom || r.model.scrollAnchor != anchor) {
										b.Fatalf("held viewport moved: follow=%v anchor=%d, want %d", r.model.followBottom, r.model.scrollAnchor, anchor)
									}
									paint := mode == "toggle-render"
									b.ReportAllocs()
									for b.Loop() {
										for range 3 {
											r.handleEvent(event)
											if paint {
												r.render()
											}
										}
									}
									b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(3*b.N), "ns/toggle")
									b.ReportMetric(float64(detailedRows), "detailed-rows")
								})
							}
						})
					}
				})
			}
		})
	}
}

func newDisclosureBenchmarkREPL(b *testing.B, count int, dense, held bool) *managedREPL {
	b.Helper()
	screen, err := headlessscreen.New(140, 40)
	if err != nil {
		b.Fatal(err)
	}
	old := ui.DefaultBackend.Screen
	ui.DefaultBackend.Screen = themedScreen{screen}
	b.Cleanup(func() {
		ui.DefaultBackend.Screen = old
		screen.Fini()
	})
	r := newManagedREPL(&Config{}, "disclosure-benchmark", 0, 0)
	b.Cleanup(func() {
		r.cancelWheelPaint()
		if err := r.closeTabs(); err != nil {
			b.Error(err)
		}
	})
	r.setupWidgets()
	r.affordanceW = &affordanceLayer{}
	m := r.model
	m.disclosures = disclosuresFolded
	var batch []toolDisclosureRow
	anchorText := fmt.Sprintf("Completed group %06d", count/2)
	for group := range count {
		if !dense || group == 0 {
			m.appendLine(fmt.Sprintf("Inspect package %06d and verify its tests.", group))
			thought := m.newReasoningRecord(true)
			m.appendReasoningTail(thought, strings.Repeat("Inspect the implementation, compare the callers, and check the multiline test output before summarizing the result.\n", 8), false)
			thought.elapsed = 2 * time.Second
			m.refreshReasoningRecord(thought, 140)
		}
		call := messages.ChatMessageToolCall{
			ID:        fmt.Sprintf("tool-%06d", group),
			Name:      "bash",
			Arguments: fmt.Sprintf(`{"command":"go test -v ./internal/package%06d"}`, group),
		}
		var output strings.Builder
		for line := range 12 {
			fmt.Fprintf(&output, "ok package%06d TestTranscriptCase%02d: completed multiline output validation; preserved the viewport and checked the expected result.\n", group, line)
		}
		row := toolDisclosureRow{callID: call.ID, label: toolLabel(call), settled: true}
		row.setCall(call)
		row.sectionKey = m.displayCatalog.ensureTool(call).key
		row.setLine(inlineToolLine{glyph: "✓", tone: "ok", modifier: "bold", meta: "12 lines", duration: "23ms"})
		row.output = newInlineToolOutput(call, messages.ChatMessage{Content: output.String()})
		if dense {
			batch = append(batch, row)
		} else {
			m.appendCompletedToolDisclosure([]toolDisclosureRow{row})
			m.appendLine(fmt.Sprintf("Completed group %06d; all checks passed.\nThe next package can now be inspected.", group))
		}
	}
	if dense {
		m.appendCompletedToolDisclosure(batch)
		// Hold below the dense batch so expansion must preserve a stable
		// prose anchor even though the expanded output is entirely offscreen.
		anchorText = "Review completed; preserve this held viewport."
		m.appendLine(anchorText + "\n" + strings.Repeat("Additional review notes remain below the viewport.\n", 80))
	}
	r.render() // Warm the initial folded layout and frame, outside the timer.
	if held {
		anchor := -1
		for i, row := range r.transcriptW.Rows {
			if strings.Contains(ui.CellsToString(row), anchorText) {
				anchor = i
				break
			}
		}
		if anchor < 0 {
			b.Fatal("held viewport anchor missing from fixture")
		}
		m.followBottom, m.scrollAnchor = false, anchor
		r.render()
	}
	return r
}
