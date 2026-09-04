package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

func TestSessionTreeNestsAgentsUnderTheirParent(t *testing.T) {
	now := time.Now()
	infos := []*sessions.Metadata{
		{Name: "omega", Parent: "gone", LastUsed: now.Add(-3 * time.Minute)},
		{Name: "epsilon", Parent: "gamma", LastUsed: now.Add(-2 * time.Minute)},
		{Name: "gamma", LastUsed: now},
		{Name: "delta", Parent: "gamma", Description: "count files", LastUsed: now.Add(-time.Minute)},
		{Name: "alpha", LastUsed: now.Add(time.Minute)},
	}
	var got []string
	for _, node := range sessionTree(infos) {
		info := infos[node.Index]
		got = append(got, sessionTreeName(info, node.Depth)+"/"+strings.Repeat("+", node.Depth)+"/"+strings.Repeat("c", node.Children))
	}
	want := []string{"alpha//", "gamma//cc", "↳ count files/+/", "↳ epsilon/+/", "omega//"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tree = %q, want %q", got, want)
	}
}

func TestSessionTreeListsParentCyclesFlat(t *testing.T) {
	infos := []*sessions.Metadata{
		{Name: "a", Parent: "b"},
		{Name: "b", Parent: "a"},
	}
	nodes := sessionTree(infos)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %#v, want both sessions listed", nodes)
	}
}

func TestListContextsNestsAgentsUnlessFlat(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	spawn := func(name, parent, description string) {
		session := testAcquireSession(t, store, name)
		metadata, err := session.GetMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Parent = parent
		metadata.Description = description
		if err := session.SetMetadata(ctx, metadata); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	spawn("delta", "gamma", "count files")
	spawn("omega", "gone", "")
	if err := testAcquireSession(t, store, "gamma").Close(); err != nil {
		t.Fatal(err)
	}

	tree := strings.Split(strings.TrimSpace(captureStdout(t, func() {
		if err := handleListContexts(ctx, store, false); err != nil {
			t.Fatal(err)
		}
	})), "\n")
	if len(tree) != 3 {
		t.Fatalf("tree lines = %q", tree)
	}
	if !strings.HasPrefix(tree[0], "gamma ") {
		t.Fatalf("parent did not lead the tree: %q", tree)
	}
	if !strings.HasPrefix(tree[1], "  ↳ delta · count files ") || strings.Contains(tree[1], "spawned by") {
		t.Fatalf("agent row = %q, want it nested under gamma with its label", tree[1])
	}
	if !strings.HasPrefix(tree[2], "omega ") || !strings.Contains(tree[2], "(spawned by gone)") {
		t.Fatalf("orphan row = %q, want it top-level naming its parent", tree[2])
	}

	flat := captureStdout(t, func() {
		if err := handleListContexts(ctx, store, true); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(flat, "↳") || !strings.Contains(flat, "\ndelta (spawned by gamma) - ") {
		t.Fatalf("flat list = %q, want one plain line per session naming the agent's parent", flat)
	}
}
