package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	readAcceptedPlanToolName = "read_accepted_plan"
	// System guidance cannot be compacted away. Larger snapshots belong in
	// on-demand tool results, not in every request for the session.
	acceptedPlanInlineBytes = 2 << 10
)

func acceptedPlanPrompt(p *sessions.Plan) string {
	if len(p.Snapshot) <= acceptedPlanInlineBytes {
		return "\n\nAccepted Markdown:\n" + p.Snapshot
	}
	return fmt.Sprintf("\n\nAccepted Markdown (%d bytes, revision %s) is not inlined. Use %s with no arguments to start reading it, and follow next byte_offset for subsequent pages. Read the approved requirements and verification details for the work before acting.", len(p.Snapshot), p.Revision, readAcceptedPlanToolName)
}

type acceptedPlanReader struct {
	tools.NativeTool
	done <-chan struct{} // also identifies this specific turn
}

type acceptedPlanReadKey struct{}

type acceptedPlanRead struct {
	done           <-chan struct{}
	revision, text string
}

// Only the parent turn's dispatch for this reader gets a capability. Do not
// attach it to the run context or worker launches; the copy has no session or
// draft handle and expires when the turn ends.
func (t *turnExecution) bindAcceptedPlanRead(ctx context.Context, call messages.ChatMessageToolCall) context.Context {
	if call.Name != readAcceptedPlanToolName || t.plan == nil || t.plan.Mode != sessions.PlanAccepted || t.runAgent == nil || askingMemberFrom(ctx) != "" {
		return ctx
	}
	registry := t.runAgent.ToolRegistry()
	if registry == nil {
		return ctx
	}
	tool, _ := registry.Get(readAcceptedPlanToolName)
	reader, ok := tool.(*acceptedPlanReader)
	if !ok || reader.done != t.ctx.Done() {
		return ctx
	}
	return context.WithValue(ctx, acceptedPlanReadKey{}, acceptedPlanRead{
		done: reader.done, revision: t.plan.Revision, text: t.plan.Snapshot,
	})
}

func (*acceptedPlanReader) GetName() string { return readAcceptedPlanToolName }
func (*acceptedPlanReader) ReadsOnly() bool { return true }
func (*acceptedPlanReader) RecallStub() string {
	return "[Accepted plan page elided; call read_accepted_plan again with the page's byte_offset.]"
}

func (*acceptedPlanReader) GetSchema() *schema.ToolSchema {
	return schema.Tool(readAcceptedPlanToolName,
		"Read a bounded raw Markdown page from this turn's immutable accepted plan, not the editable draft. Follow the reported next byte_offset to continue.",
		schema.Params{"byte_offset": schema.Int("0-based byte position at a UTF-8 boundary (default 0); use the returned next byte_offset to continue")})
}

func (r *acceptedPlanReader) Execute(ctx context.Context, raw map[string]any) (string, error) {
	read, ok := ctx.Value(acceptedPlanReadKey{}).(acceptedPlanRead)
	if !ok || read.done == nil || read.done != r.done || askingMemberFrom(ctx) != "" {
		return "", tools.NewToolError("No accepted plan read capability for this turn", "PLAN_READ_UNAVAILABLE")
	}
	select {
	case <-read.done:
		return "", tools.NewToolError("The accepted plan turn has ended", "PLAN_READ_UNAVAILABLE")
	default:
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	offset := 0
	if len(raw) != 0 {
		value, ok := raw["byte_offset"]
		if !ok || len(raw) != 1 {
			return "", tools.NewToolError("Only byte_offset may be supplied", "PLAN_READ_ARGUMENTS")
		}
		offset = tools.Args(raw).Int("byte_offset", -1)
		switch value := value.(type) {
		case float64:
			if value != float64(offset) {
				offset = -1
			}
		case int64:
			if value != int64(offset) {
				offset = -1
			}
		}
	}
	if offset < 0 || offset > len(read.text) {
		return "", tools.NewToolError(fmt.Sprintf("byte_offset must be a whole number from 0 to %d", len(read.text)), "PLAN_READ_ARGUMENTS")
	}
	if offset < len(read.text) && !utf8.RuneStart(read.text[offset]) {
		return "", tools.NewToolError("byte_offset splits a UTF-8 character; use a page's reported next byte_offset", "PLAN_READ_ARGUMENTS")
	}
	// A too-small cap could pinch the header, continuation, or the first
	// rune and repeat the same offset. Real agent page caps are larger; 256
	// bytes fits the metadata for a 64-digit revision and a MaxPlanBytes plan.
	if tools.PageBytes(ctx) < 256 {
		return "", tools.NewToolError("Page budget is too small to read the accepted plan", "PLAN_READ_BUDGET")
	}
	return tools.PageByteWindow(ctx, strings.NewReader(read.text), "accepted plan", read.revision, int64(len(read.text)), int64(offset))
}
