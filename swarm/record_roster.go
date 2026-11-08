package swarm

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/sessions"
)

// compactRosterRecords supplies only the initial assignment's roster. Counts
// and at most 32 working members share one snapshot; it is not a public status
// projection or an authority source. Legacy access retains whole-family costs.
func (r *Runtime) compactRosterRecords(ctx context.Context) (string, error) {
	var text string
	err := r.readRecords(ctx, func(rd *recordReader) error {
		counts := make(map[Lifecycle]int, 4)
		for _, state := range []Lifecycle{LifecycleActive, LifecycleWaiting, LifecycleIdle, LifecyclePaused} {
			page, err := rd.raw.Query(sessions.RecordQuery{Kind: "member", Index: sessions.CoordinationIndexMemberLifecycle, Key: string(state), CountOnly: true, IncludeTotal: true})
			if err != nil {
				return err
			}
			counts[state] = page.Total
		}
		working, idle, paused := counts[LifecycleActive]+counts[LifecycleWaiting], counts[LifecycleIdle], counts[LifecyclePaused]
		var b strings.Builder
		fmt.Fprintf(&b, "Roster at assignment start (use list_agents to refresh): %d members: %d working, %d idle, %d paused.\n", working+idle+paused, working, idle, paused)
		if working == 0 {
			b.WriteString("Working now: none.\n")
			text = b.String()
			return nil
		}
		// The first 32 of each ordered partition contain the first 32 of
		// their union. Do not fetch inactive members to fill this page.
		var selected []*Member
		for _, state := range []Lifecycle{LifecycleActive, LifecycleWaiting} {
			page, err := rd.Members(sessions.RecordQuery{Index: sessions.CoordinationIndexMemberLifecycle, Key: string(state), Limit: 32})
			if err != nil {
				return err
			}
			for _, record := range page.Records {
				selected = append(selected, record.Value)
			}
		}
		sort.Slice(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
		selected = selected[:min(len(selected), 32)]
		b.WriteString("Working now:\n")
		for _, m := range selected {
			e, err := rd.Execution(m.Execution)
			if err != nil {
				return err
			}
			state, detail := memberActivity(m, e, "")
			deferred, err := rd.memberTaskDeferred(m)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s · %s · %s · task %s\n", agentName(m), m.Label, DisplayLabel(state, detail, deferred), m.Task)
		}
		if working > len(selected) {
			fmt.Fprintf(&b, "%d additional working members available through list_agents.\n", working-len(selected))
		}
		text = b.String()
		return nil
	})
	return text, err
}

func (rd *recordReader) memberTaskDeferred(m *Member) (bool, error) {
	if m.Task == "" {
		return false, nil
	}
	t, err := rd.Task(m.Task)
	if err != nil || t == nil || t.Deferral == nil || t.Status == "done" || t.Status == "canceled" || t.Execution == "" || t.Deferral.Workflow == "" {
		return false, err
	}
	e, err := rd.Execution(t.Execution)
	if err != nil {
		return false, err
	}
	w, err := rd.WorkflowHeader(t.Deferral.Workflow)
	if err != nil || w == nil {
		return false, err
	}
	return deferredTaskExecution(t, e, w.Run, w.Status, w.Acknowledged), nil
}
