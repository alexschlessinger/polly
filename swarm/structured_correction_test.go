package swarm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A correction counts as delivered by the marker the message carries, since
// its content may have been bounded to fit the context budget.
func TestCorrectionDeliveryIsMatchedByItsMarker(t *testing.T) {
	t.Parallel()
	s := &structuredResultState{corrections: 1, correctionText: "Invalid final result: fix it"}
	e := &Execution{Request: AgentRequest{Schema: map[string]any{"type": "object"}}}
	bounded := s.correctionMessage(s.correctionText)
	bounded.Content = "[message stored as artifact sha256:abc; 9000 bytes; 1 lines. Use read_artifact to read it in full.]"
	if err := s.applyCheckpoint(&State{}, e, []messages.ChatMessage{bounded}); err != nil {
		t.Fatal(err)
	}
	if e.ResultCorrections != 1 || e.PendingResultCorrection != "" {
		t.Fatalf("bounded correction not delivered: corrections=%d pending=%q", e.ResultCorrections, e.PendingResultCorrection)
	}
	// An earlier correction, its count restored from persisted metadata,
	// does not deliver a later one; the later one does.
	s.corrections, s.correctionText = 2, "Invalid final result: again"
	older := s.correctionMessage("older")
	older.Metadata[resultCorrectionKey] = float64(1)
	if err := s.applyCheckpoint(&State{}, e, []messages.ChatMessage{older}); err != nil || e.PendingResultCorrection != s.correctionText {
		t.Fatalf("older correction delivered the pending one: pending=%q err=%v", e.PendingResultCorrection, err)
	}
	current := s.correctionMessage("current")
	current.Metadata[resultCorrectionKey] = float64(2)
	if err := s.applyCheckpoint(&State{}, e, []messages.ChatMessage{current}); err != nil || e.PendingResultCorrection != "" {
		t.Fatalf("current correction not delivered: pending=%q err=%v", e.PendingResultCorrection, err)
	}
}
