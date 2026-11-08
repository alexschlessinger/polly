package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func hostAgentHistory(background, succeeded, denied bool) []messages.ChatMessage {
	args := `{"label":"child task","background":false}`
	if background {
		args = `{"label":"child task","background":true}`
	}
	call := agentCall("launch", args)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "delegate"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}},
		{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: "success"},
		{Role: messages.MessageRoleAssistant, Content: "launched"},
	}
	history[2].SetToolSucceeded(succeeded)
	if denied {
		history[2].Content = llm.ToolDeniedContent
	}
	return history
}

func hostAgentRow(t *testing.T, m *replModel) *toolDisclosureRow {
	t.Helper()
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			if record.rows[i].callID == "launch" {
				return &record.rows[i]
			}
		}
	}
	t.Fatal("missing launch row")
	return nil
}

func TestHostSummaryEnrichmentRecomputesOwnedLaunchState(t *testing.T) {
	for _, outcome := range []string{"background", "foreground", "failed", "denied"} {
		for _, condition := range []string{"deleted", "ambiguous"} {
			t.Run(outcome+"/"+condition, func(t *testing.T) {
				info := &sessions.SessionView{Metadata: &sessions.Metadata{Name: "parent"}, History: hostAgentHistory(outcome != "foreground", outcome != "failed", outcome == "denied")}
				saved := prepareChildDisplay(info, &Config{}, 80)
				row := hostAgentRow(t, saved)
				// These represent data already owned by the transcript/display,
				// not fields a summary is allowed to erase on invalidation.
				row.agent.viewID, row.agent.session = "transcript-id", "transcript-name"
				row.agent.workflowID, row.agent.workflowName = "workflow-id", "workflow name"
				want := *row.agent
				wantCall, wantDisplay := row.callID, row.displayID
				var recordIDs []int64
				for _, record := range saved.toolDisclosures.all() {
					recordIDs = append(recordIDs, record.id)
				}
				counts := saved.agentCounts(recordIDs)
				wantLocal := map[string]string{"background": "unknown", "foreground": "done", "failed": "failed", "denied": "denied"}[outcome]
				if counts.Total != 1 || row.agent.local != wantLocal {
					t.Fatalf("launch fixture: counts=%+v local=%q want=%q", counts, row.agent.local, wantLocal)
				}
				summary := sessions.SessionSummary{ID: "summary-id", Metadata: &sessions.Metadata{Name: "summary-name", Parent: "parent", SpawnCallID: "launch", SpawnOutcome: sessions.ReportFinished}}
				saved.hydrateAgentSessions("parent", []sessions.SessionSummary{summary})
				before := *row.agent
				if before.sessionSummaryBase == nil || before.viewID != summary.ID {
					t.Fatal("summary did not enrich fixture")
				}
				next := refreshSavedDisplayMetadata(context.Background(), saved, info, &Config{})
				var fresh []sessions.SessionSummary
				if condition == "ambiguous" {
					duplicate := summary
					duplicate.ID = "duplicate"
					fresh = []sessions.SessionSummary{summary, duplicate}
				}
				next.hydrateAgentSessions("parent", fresh)
				gotRow := hostAgentRow(t, next)
				if !reflect.DeepEqual(*gotRow.agent, want) || gotRow.callID != wantCall || gotRow.displayID != wantDisplay {
					t.Fatalf("invalidated summary changed transcript facts: got=%+v want=%+v", gotRow.agent, want)
				}
				if got := next.agentCounts(recordIDs); !reflect.DeepEqual(got, counts) {
					t.Fatalf("counts=%+v want=%+v", got, counts)
				}
				if !reflect.DeepEqual(*row.agent, before) || row.agent == gotRow.agent {
					t.Fatal("metadata refresh mutated or aliased previous saved row")
				}
				// A later successful refresh must start from the same base, not
				// layer stale metadata over the previously enriched state.
				next.hydrateAgentSessions("parent", []sessions.SessionSummary{summary})
				next.hydrateAgentSessions("parent", nil)
				if !reflect.DeepEqual(*hostAgentRow(t, next).agent, want) {
					t.Fatal("repeated enrichment lost launch state")
				}
			})
		}
	}
}

func TestHostMetadataOnlyInspectorChildBindingColdParity(t *testing.T) {
	for _, condition := range []string{"deleted", "ambiguous"} {
		t.Run(condition, func(t *testing.T) {
			ctx := context.Background()
			base := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, base, "root")
			parent := testAcquireSession(t, base, "parent")
			parentID := parent.(sessions.ViewIdentity).ViewID()
			addChild := func(name string) {
				child, err := base.Acquire(ctx, name, sessions.AcquireOptions{Parent: "parent"})
				if err != nil {
					t.Fatal(err)
				}
				md, err := child.GetMetadata(ctx)
				if err != nil {
					t.Fatal(err)
				}
				md.SpawnCallID, md.SpawnOutcome = "launch", sessions.ReportFinished
				if err := child.SetMetadata(ctx, md); err != nil {
					t.Fatal(err)
				}
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
			}
			addChild("first-child")
			if err := parent.Close(); err != nil {
				t.Fatal(err)
			}
			pages := newDisplayPageStore(0)
			pages.history = hostAgentHistory(true, true, false)
			pages.header.ID, pages.header.Sequence, pages.header.Metadata.Name = parentID, int64(len(pages.history)), "parent"
			r.state.sessionStore = &suppliedDisplayStore{SessionStore: base, displayPageStore: pages}
			r.inspect(viewTarget{session: sessions.ViewTarget{ID: parentID}})
			v := waitInspector(t, r, 140)
			oldSaved := v.saved
			oldAgent := *hostAgentRow(t, oldSaved).agent
			before := len(pages.queries)
			if condition == "deleted" {
				if err := base.Delete(ctx, "first-child"); err != nil {
					t.Fatal(err)
				}
			} else {
				addChild("second-child")
			}
			pages.header.Revision, pages.header.Metadata.Title = "new", "new title"
			r.inspectorRefreshAt = time.Time{}
			r.refreshInspector(140)
			settleInspectorWork(r)
			if len(pages.queries) != before || v.saved == oldSaved {
				t.Fatal("refresh did not reuse immutable history")
			}
			info := sessionViewFromHeader(&pages.header)
			info.History = pages.history
			cold := prepareChildDisplay(info, &Config{}, 80)
			summaries, err := sessionSummaries(ctx, base, &parentID)
			if err != nil {
				t.Fatal(err)
			}
			cold.hydrateAgentSessions("parent", summaries)
			got, want := hostAgentRow(t, v.saved).agent, hostAgentRow(t, cold).agent
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("metadata-only/cold parity: got=%+v want=%+v", got, want)
			}
			if !reflect.DeepEqual(*hostAgentRow(t, oldSaved).agent, oldAgent) {
				t.Fatal("previous saved model was mutated")
			}
		})
	}
}
