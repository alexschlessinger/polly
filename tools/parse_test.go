package tools

import (
	"testing"
)

func TestParseServerSpec_NoHash(t *testing.T) {
	file, server := ParseServerSpec("path/to/config.json")
	if file != "path/to/config.json" || server != "" {
		t.Errorf("ParseServerSpec(no hash) = (%q, %q), want (%q, %q)",
			file, server, "path/to/config.json", "")
	}
}

func TestParseServerSpec_WithHash(t *testing.T) {
	file, server := ParseServerSpec("path/config.json#myserver")
	if file != "path/config.json" || server != "myserver" {
		t.Errorf("ParseServerSpec(with hash) = (%q, %q), want (%q, %q)",
			file, server, "path/config.json", "myserver")
	}
}

func TestParseServerSpec_HashNotAfterJson(t *testing.T) {
	file, server := ParseServerSpec("path/my#weird.json")
	if file != "path/my#weird.json" || server != "" {
		t.Errorf("ParseServerSpec(hash not after .json) = (%q, %q), want (%q, %q)",
			file, server, "path/my#weird.json", "")
	}
}

func TestParseServerSpec_Empty(t *testing.T) {
	file, server := ParseServerSpec("")
	if file != "" || server != "" {
		t.Errorf("ParseServerSpec(empty) = (%q, %q), want (%q, %q)",
			file, server, "", "")
	}
}

func TestExtractNamespace_FilePath(t *testing.T) {
	ns := extractNamespace("/path/to/filesystem.json")
	if ns != "filesystem" {
		t.Errorf("extractNamespace(file path) = %q, want %q", ns, "filesystem")
	}
}

func TestExtractNamespace_WithServer(t *testing.T) {
	ns := extractNamespace("/path/mcp.json#myserver")
	if ns != "myserver" {
		t.Errorf("extractNamespace(with server) = %q, want %q", ns, "myserver")
	}
}
