package subagent

import (
	"github.com/alexschlessinger/pollytool/tools"
	"testing"
)

func TestParseReview(t *testing.T) {
	req, err := parseRequest(tools.Args{"label": "Test agent", "task": "inspect", "read_only": true, "review": true})
	if err != nil || !req.Review || !req.ReadOnly {
		t.Fatalf("request=%+v error=%v", req, err)
	}
	if _, err := parseRequest(tools.Args{"label": "Test agent", "task": "edit", "review": true}); err == nil {
		t.Fatal("editing review accepted")
	}
	// The runtime checks saved authority for continuations.
	if _, err := parseRequest(tools.Args{"label": "Test agent", "task": "inspect", "review": true, "session": "existing"}); err != nil {
		t.Fatal(err)
	}
}
