package messages

import (
	"context"
	"reflect"
	"testing"
)

func TestAssistantBlocksSeparateCommentaryAndPreserveUnphasedBoundaries(t *testing.T) {
	ch := make(chan ChatMessage, 6)
	for _, block := range []AssistantText{{ID: "a", Phase: PhaseCommentary, Text: "same"}, {ID: "b", Phase: PhaseFinalAnswer, Text: "sa"}, {ID: "b", Phase: PhaseFinalAnswer, Text: "me"}, {ID: "c", Text: "additional"}} {
		ch <- ChatMessage{TextBlocks: []AssistantText{block}}
	}
	ch <- ChatMessage{StopReason: StopReasonEndTurn}
	close(ch)
	var final *ChatMessage
	var content, progress string
	starts := 0
	for e := range NewStreamProcessor().ProcessMessagesToEvents(context.Background(), ch) {
		switch e.Type {
		case EventTypeContent:
			content += e.Content
		case EventTypeCommentary:
			progress += e.Content
		case EventTypeComplete:
			final = e.Message
		}
		if e.TextStart {
			starts++
		}
	}
	if final == nil || final.Content != "same\n\nadditional" || content != final.Content || progress != "same" || starts != 3 {
		t.Fatalf("reply=%#v content=%q progress=%q starts=%d", final, content, progress, starts)
	}
	if !final.HasTextBlocks() || final.ModelText() != "same\n\nsame\n\nadditional" {
		t.Fatalf("model text=%q", final.ModelText())
	}
	clone := final.Clone()
	clone.TextBlocks[0].Text = "changed"
	if reflect.DeepEqual(clone.TextBlocks, final.TextBlocks) || final.TextBlocks[0].Text != "same" {
		t.Fatal("clone shares blocks")
	}
	flattened := final.Clone()
	flattened.FlattenAssistantText()
	if flattened.Content != final.ModelText() || len(flattened.TextBlocks) != 0 {
		t.Fatalf("flattened=%#v", flattened)
	}
	final.Content = "replacement"
	if final.HasTextBlocks() || final.ModelText() != "replacement" {
		t.Fatal("stale blocks override an edit")
	}
}
