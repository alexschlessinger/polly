package main

import (
	"sort"

	"github.com/alexschlessinger/pollytool/sessions"
)

// sessionTreeNode places one session in the parent tree: Index is its
// position in the slice given to sessionTree, Depth is 0 for a session with
// no listed parent and grows under the session that spawned it, and Children
// counts the agents listed beneath it.
type sessionTreeNode struct {
	Index    int
	Depth    int
	Children int
}

// sessionTree orders sessions as a tree: the sessions with no listed parent
// by last use, newest first, each followed by the agents it spawned, ordered
// the same way. An agent whose parent is gone stands on its own.
func sessionTree(infos []*sessions.Metadata) []sessionTreeNode {
	order := make([]int, len(infos))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := infos[order[a]], infos[order[b]]
		if x.LastUsed.Equal(y.LastUsed) {
			return x.Name < y.Name
		}
		return x.LastUsed.After(y.LastUsed)
	})
	byName := make(map[string]int, len(infos))
	for i, info := range infos {
		byName[info.Name] = i
	}
	children := make(map[int][]int)
	roots := make([]int, 0, len(infos))
	for _, i := range order {
		info := infos[i]
		if parent, ok := byName[info.Parent]; ok && info.Parent != "" && parent != i {
			children[parent] = append(children[parent], i)
			continue
		}
		roots = append(roots, i)
	}
	nodes := make([]sessionTreeNode, 0, len(infos))
	placed := make(map[int]bool, len(infos))
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if placed[i] {
			return
		}
		placed[i] = true
		nodes = append(nodes, sessionTreeNode{Index: i, Depth: depth, Children: len(children[i])})
		for _, child := range children[i] {
			walk(child, depth+1)
		}
	}
	for _, i := range roots {
		walk(i, 0)
	}
	// Sessions naming each other as parents have no root; list them flat.
	for _, i := range order {
		walk(i, 0)
	}
	return nodes
}

// sessionTreeName is what a session is listed as: an agent goes by the
// label of its brief under its parent, everything else by its name.
func sessionTreeName(info *sessions.Metadata, depth int) string {
	name := sessions.DisplayLabel(info)
	if depth > 0 {
		return "↳ " + name
	}
	return name
}

// orderSessionGroups puts the subtrees rooted at open workspaces first, in
// workspace order, ahead of the saved sessions in their tree order. Within an
// open subtree the agents sort by priority (lower first) when they are all
// direct children, so the one waiting on an approval leads.
func orderSessionGroups(nodes []sessionTreeNode, workspaceIndex func(sessionTreeNode) (int, bool), priority func(sessionTreeNode) int) []sessionTreeNode {
	type group struct {
		nodes []sessionTreeNode
		order int
		open  bool
	}
	var groups []group
	for _, node := range nodes {
		if node.Depth == 0 || len(groups) == 0 {
			g := group{}
			g.order, g.open = workspaceIndex(node)
			groups = append(groups, g)
		}
		groups[len(groups)-1].nodes = append(groups[len(groups)-1].nodes, node)
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.open != b.open {
			return a.open
		}
		return a.open && a.order < b.order
	})
	out := make([]sessionTreeNode, 0, len(nodes))
	for _, g := range groups {
		children := g.nodes[1:]
		flat := true
		for _, child := range children {
			flat = flat && child.Depth == 1
		}
		if g.open && flat {
			sort.SliceStable(children, func(i, j int) bool { return priority(children[i]) < priority(children[j]) })
		}
		out = append(out, g.nodes...)
	}
	return out
}
