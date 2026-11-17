package tools

import "testing"

// The tools that only read say so, through a namespace wrapper as well. A
// tool that can act, or that says nothing, does not.
func TestToolsThatOnlyReadSaySo(t *testing.T) {
	registry := NewToolRegistry(nil, WithNativeTools(), WithUnsafeNoSandbox())
	defer registry.Close()
	for name, want := range map[string]bool{
		"read_file": true, "list_dir": true, "view_image": true,
		"bash": false, "write_file": false, "edit_file": false,
	} {
		if _, err := registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s is not loaded", name)
		}
		if ReadsOnly(tool) != want {
			t.Errorf("ReadsOnly(%s) = %v, want %v", name, !want, want)
		}
	}
	look, work := &Func{Name: "look", ReadOnly: true}, &Func{Name: "work"}
	if !ReadsOnly(&NamespacedTool{Tool: look}) || ReadsOnly(&NamespacedTool{Tool: work}) || ReadsOnly(work) {
		t.Error("a namespace wrapper changed what its tool says")
	}
}
