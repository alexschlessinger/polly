package openrouter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

func TestReasoningPolicyPresence(t *testing.T) {
	for _, tc := range []struct {
		policy   string
		complete bool
		efforts  []string
	}{
		{`{}`, false, nil}, {`{"supported_efforts":null}`, true, nil}, {`{"supported_efforts":[]}`, true, []string{}}, {`{"supported_efforts":["low"]}`, true, []string{"low"}},
	} {
		var policy map[string]any
		json.Unmarshal([]byte(tc.policy), &policy)
		caps := contract.ModelCapabilities{}
		decodeReasoningPolicy(&caps, policy)
		raw, _ := json.Marshal(caps)
		var persisted contract.ModelCapabilities
		json.Unmarshal(raw, &persisted)
		if persisted.ReasoningEffortsComplete != tc.complete || !reflect.DeepEqual(persisted.ReasoningEfforts, tc.efforts) {
			t.Fatalf("lost presence %s: %+v", tc.policy, persisted)
		}
	}
}
