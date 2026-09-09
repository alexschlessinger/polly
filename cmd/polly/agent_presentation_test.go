package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
)

func TestAgentActivityLineGlyphsByLifecycle(t *testing.T) {
	local := func(word string, busy bool) *agentActivity {
		a := &agentActivity{}
		a.setLocal(word, busy)
		return a
	}
	for _, tc := range []struct {
		name  string
		row   *agentActivity
		glyph string
	}{
		{"approval outranks everything", &agentActivity{approval: true, state: swarm.AgentPresentation{Lifecycle: swarm.LifecycleActive, Busy: true}}, "!"},
		{"busy member", &agentActivity{state: swarm.AgentPresentation{Lifecycle: swarm.LifecycleWaiting, Busy: true}}, " "},
		{"failed member", &agentActivity{state: swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Outcome: "failed"}}, "✗"},
		{"interrupted member", &agentActivity{state: swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Outcome: "paused"}}, "·"},
		{"accepted member", &agentActivity{state: swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, TaskStatus: "done"}}, "✓"},
		{"submitted member", &agentActivity{state: swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, TaskStatus: "awaiting review"}}, "·"},
		{"launch starting", local("starting", true), " "},
		{"launch done", local("done", false), "✓"},
		{"launch denied", local("denied", false), "✗"},
		{"launch canceled", local("canceled", false), "·"},
	} {
		if glyph, _ := agentGlyph(tc.row); glyph != tc.glyph {
			t.Errorf("%s: glyph %q, want %q", tc.name, glyph, tc.glyph)
		}
	}
}

func TestSpawnOutcomeStateUsesLifecycleVocabulary(t *testing.T) {
	for outcome, want := range map[sessions.ReportStatus]struct {
		lifecycle swarm.Lifecycle
		display   string
	}{
		sessions.ReportFinished: {swarm.LifecycleIdle, "idle · done"},
		sessions.ReportFailed:   {swarm.LifecyclePaused, "paused · failed"},
		sessions.ReportCanceled: {swarm.LifecyclePaused, "paused · interrupted"},
		sessions.ReportPaused:   {swarm.LifecyclePaused, "paused · iteration limit"},
		"":                      {swarm.LifecycleIdle, "unknown"},
	} {
		p := spawnOutcomeState(outcome)
		if p.Lifecycle != want.lifecycle || p.Display != want.display || p.Busy || spawnOutcomeStatus(outcome) != want.display {
			t.Errorf("%q: %+v", outcome, p)
		}
	}
}

func TestActivityAgentCountsByLifecycle(t *testing.T) {
	var counts activityAgentCounts
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecycleActive, Busy: true})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecycleWaiting, Busy: true})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Outcome: "failed"})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Outcome: "paused"})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecyclePaused, Control: swarm.MemberControlStopped, Outcome: "completed"})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, TaskStatus: "done"})
	counts.add(swarm.AgentPresentation{Lifecycle: swarm.LifecycleIdle, TaskStatus: "awaiting review", Deferred: true})
	if counts != (activityAgentCounts{Total: 7, Running: 2, Failed: 1, Paused: 2, Deferred: 1}) {
		t.Fatalf("typed buckets: %+v", counts)
	}
	var local activityAgentCounts
	local.addOutcome("starting", true)
	local.addOutcome("canceled", false)
	local.addOutcome("denied", false)
	local.addOutcome("paused · iteration limit", false)
	local.addOutcome("unknown", false)
	if local != (activityAgentCounts{Total: 5, Running: 1, Canceled: 1, Failed: 1, Paused: 1}) {
		t.Fatalf("launch buckets: %+v", local)
	}
}
