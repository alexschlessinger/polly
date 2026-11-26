package rex

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveRexIntegration(t *testing.T) {
	if os.Getenv("POLLYTOOL_REX_LIVE_TESTS") != "1" {
		t.Skip("set POLLYTOOL_REX_LIVE_TESTS=1 to run live Rex integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := NewClient(WithTimeout(10 * time.Second))

	whoami, err := client.Whoami(ctx)
	if err != nil {
		t.Fatalf("Whoami: %v", err)
	}
	if whoami.RexVersion == "" {
		t.Fatalf("expected non-empty RexVersion: %+v", whoami)
	}

	ver, err := client.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if ver.APIRevision <= 0 {
		t.Fatalf("expected positive APIRevision: %+v", ver)
	}

	if _, err := client.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}

	apiList, err := client.ListMethods(ctx)
	if err != nil {
		t.Fatalf("ListMethods: %v", err)
	}
	if len(apiList.Methods) == 0 {
		t.Fatalf("expected non-empty server methods")
	}

	doc, err := client.DescribeMethod(ctx, "session.new_split")
	if err != nil {
		t.Fatalf("DescribeMethod: %v", err)
	}
	if doc.Name != "session.new_split" {
		t.Fatalf("DescribeMethod name = %q", doc.Name)
	}

	types, err := client.BlockTypes(ctx)
	if err != nil {
		t.Fatalf("BlockTypes: %v", err)
	}
	if len(types.Creators) == 0 {
		t.Fatalf("expected at least one block creator")
	}

	if _, err := client.ListSessions(ctx); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if _, err := client.ListHosts(ctx); err != nil {
		t.Fatalf("ListHosts: %v", err)
	}

	if whoami.SessionID != "" {
		view, err := client.InspectSession(ctx, whoami.SessionID)
		if err != nil {
			t.Fatalf("InspectSession: %v", err)
		}
		if view.SessionID != whoami.SessionID {
			t.Fatalf("InspectSession returned %q, want %q", view.SessionID, whoami.SessionID)
		}
		if whoami.BlockID != "" {
			if _, err := client.Modes(ctx, whoami.SessionID, whoami.BlockID); err != nil {
				t.Fatalf("Modes: %v", err)
			}
			if _, err := client.Title(ctx, whoami.SessionID, whoami.BlockID); err != nil {
				t.Fatalf("Title: %v", err)
			}
			if _, err := client.Size(ctx, whoami.SessionID, whoami.BlockID); err != nil {
				t.Fatalf("Size: %v", err)
			}
			if _, err := client.ProgramStatus(ctx, whoami.SessionID, whoami.BlockID); err != nil {
				t.Fatalf("ProgramStatus: %v", err)
			}
		}
	}

	var luaOut struct {
		Sum int `json:"sum"`
	}
	if err := client.EvalLua(ctx, `return {sum = (rex.args.n or 0) + 1}`, LuaOptions{
		Args: map[string]any{"n": 41},
	}, &luaOut); err != nil {
		t.Fatalf("EvalLua: %v", err)
	}
	if luaOut.Sum != 42 {
		t.Fatalf("EvalLua sum = %d, want 42", luaOut.Sum)
	}
}
