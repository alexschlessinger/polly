package swarm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
)

// ErrCallbackOwnership means host callbacks conflict with the runtime's sole
// ownership of durable input admission, checkpoints, or tool intent journals.
// Direct llm.Agent.Run callers may supply all three hooks themselves.
var ErrCallbackOwnership = errors.New("swarm owns execution persistence callbacks")

// copyHostCallbacks snapshots the host's hooks before runtime binding. The
// persistence hooks cannot be composed: each must have one owner for the input
// receipts, generated prefix, and in-flight tool intent to commit together.
func copyHostCallbacks(host *llm.AgentCallbacks) (llm.AgentCallbacks, error) {
	if host == nil {
		return llm.AgentCallbacks{}, nil
	}
	cb := *host
	var conflicts []string
	if cb.AdmitInput != nil {
		conflicts = append(conflicts, "AdmitInput")
	}
	if cb.Checkpoint != nil {
		conflicts = append(conflicts, "Checkpoint")
	}
	if cb.JournalToolBatch != nil {
		conflicts = append(conflicts, "JournalToolBatch")
	}
	if len(conflicts) != 0 {
		return llm.AgentCallbacks{}, fmt.Errorf("%w: %s", ErrCallbackOwnership, strings.Join(conflicts, ", "))
	}
	return cb, nil
}
