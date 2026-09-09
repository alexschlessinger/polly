package llm

import (
	"context"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// projectMessages projects without a cross-request cache: what one request
// sees when nothing about the history has been seen before.
func projectMessages(ctx context.Context, history []messages.ChatMessage, maxTokens int, store artifacts.Store, transcriptReadable bool) ([]messages.ChatMessage, ProjectionStats, error) {
	return projectMessagesCached(ctx, history, maxTokens, store, builtinProjectionTools(transcriptReadable), nil)
}

// builtinProjectionTools describes an agent with the standard built-ins.
func builtinProjectionTools(transcriptReadable bool) projectionTools {
	return projectionTools{
		transcriptReadable: transcriptReadable,
		recall:             recallStubsFor([]tools.Tool{&readArtifactTool{}, &listArtifactsTool{}, &readTranscriptTool{}}),
	}
}

func recallResultStub(name string) string {
	return builtinProjectionTools(false).recall[name]
}

// demotedToolResultForm materializes a standalone demotion. The projection's
// budget gate uses planToolDemotion instead, deferring payload work until a
// result is actually stored.
func demotedToolResultForm(msg messages.ChatMessage, hasStore bool) (string, *artifacts.Blob, bool) {
	plan := planToolDemotion(msg, hasStore, recallResultStub(msg.ToolName))
	if !plan.ok {
		return "", nil, false
	}
	if !plan.inline {
		return plan.content, nil, true
	}
	blob := &artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: toolArtifactName(msg), Data: []byte(msg.Content)}
	ref := artifacts.RefForBlob(*blob)
	return appendArtifactDescriptors(artifactReceipt(ref), msg, ref.ID, " "), blob, true
}

func estimateProjectedTokens(history []messages.ChatMessage) int {
	total := 0
	for _, msg := range history {
		total += estimateProjectedMessageTokens(msg)
	}
	return total
}
