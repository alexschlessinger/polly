package llm

import (
	"context"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// projectMessages projects history for one request without cross-request
// reuse, through the view an agent with the standard built-ins sends.
func projectMessages(ctx context.Context, history []messages.ChatMessage, store artifacts.Store, transcriptReadable bool) ([]messages.ChatMessage, ProjectionStats, error) {
	req := &CompletionRequest{Messages: contextView(history, builtinProjectionTools(transcriptReadable))}
	return projectCompletionRequest(ctx, req, store, nil)
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
