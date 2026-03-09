package skills

import (
	"archive/tar"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandPathRejectsNamedHomeExpansion(t *testing.T) {
	if _, err := ExpandPath("~otheruser/skills"); err == nil || !strings.Contains(err.Error(), "unsupported home-directory expansion") {
		t.Fatalf("ExpandPath() error = %v, want unsupported expansion error", err)
	}
}

func TestExpandPathExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir() error = %v", err)
	}

	got, err := ExpandPath("~/foo/bar")
	if err != nil {
		t.Fatalf("ExpandPath() error = %v", err)
	}
	want := filepath.Join(home, "foo", "bar")
	if got != want {
		t.Fatalf("ExpandPath(~/foo/bar) = %q, want %q", got, want)
	}
}

func TestResolveDirsDeduplicates(t *testing.T) {
	dir := t.TempDir()
	dirs, err := ResolveDirs([]string{dir, dir, dir})
	if err != nil {
		t.Fatalf("ResolveDirs() error = %v", err)
	}
	if len(dirs) != 1 {
		t.Fatalf("ResolveDirs() returned %d dirs, want 1", len(dirs))
	}
}

func TestLoadCatalogReturnsNilForEmptyDir(t *testing.T) {
	dir := t.TempDir()
	catalog, err := LoadCatalog([]string{dir})
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if catalog != nil {
		t.Fatal("LoadCatalog() should return nil for empty dir")
	}
}

func TestResolveSkillLocalPath(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "local-skill", "a local skill")

	resolved, err := ResolveSkill(filepath.Join(root, "local-skill"))
	if err != nil {
		t.Fatalf("ResolveSkill() error = %v", err)
	}
	if resolved.Name != "local-skill" {
		t.Fatalf("Name = %q, want %q", resolved.Name, "local-skill")
	}
}

func TestResolveSkillLocalPathMissingSKILLmd(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveSkill(dir)
	if err == nil || !strings.Contains(err.Error(), "missing SKILL.md") {
		t.Fatalf("ResolveSkill() error = %v, want missing SKILL.md error", err)
	}
}

func TestResolveSkillLocalPathNotADir(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "not-a-dir")
	os.WriteFile(file, []byte("hi"), 0644)

	_, err := ResolveSkill(file)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("ResolveSkill() error = %v, want not-a-directory error", err)
	}
}

func TestResolveSkillArchiveURL(t *testing.T) {
	// Create a tar.gz containing a skill directory.
	root := t.TempDir()
	createTestSkill(t, root, "archive-skill", "from an archive")

	archivePath := filepath.Join(t.TempDir(), "skill.tar.gz")
	createTarGz(t, archivePath, root, "archive-skill")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, archivePath)
	}))
	defer server.Close()

	resolved, err := ResolveSkill(server.URL + "/skill.tar.gz")
	if err != nil {
		t.Fatalf("ResolveSkill() error = %v", err)
	}
	if resolved.Name != "archive-skill" {
		t.Fatalf("Name = %q, want %q", resolved.Name, "archive-skill")
	}
	// Verify SKILL.md exists in cached dir.
	if _, err := os.Stat(filepath.Join(resolved.Dir, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md missing in resolved dir: %v", err)
	}

	// Second resolve should hit cache (no HTTP request needed).
	resolved2, err := ResolveSkill(server.URL + "/skill.tar.gz")
	if err != nil {
		t.Fatalf("cached ResolveSkill() error = %v", err)
	}
	if resolved2.Dir != resolved.Dir {
		t.Fatalf("cached dir = %q, want %q", resolved2.Dir, resolved.Dir)
	}
}

func TestIsGitURL(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://github.com/user/repo.git", true},
		{"https://github.com/user/repo", true},
		{"https://gitlab.com/user/repo", true},
		{"https://example.com/skill.tar.gz", false},
		{"/local/path", false},
	}
	for _, tt := range tests {
		if got := isGitURL(tt.url); got != tt.want {
			t.Errorf("isGitURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestParseGitTreeURL(t *testing.T) {
	tests := []struct {
		url     string
		want    *gitTreeURL
		wantNil bool
	}{
		{
			url: "https://github.com/anthropics/skills/tree/main/skills/pdf/",
			want: &gitTreeURL{
				RepoURL: "https://github.com/anthropics/skills",
				Ref:     "main",
				Subpath: "skills/pdf",
			},
		},
		{
			url: "https://github.com/anthropics/skills/tree/main/skills/pdf",
			want: &gitTreeURL{
				RepoURL: "https://github.com/anthropics/skills",
				Ref:     "main",
				Subpath: "skills/pdf",
			},
		},
		{
			url: "https://gitlab.com/user/repo/tree/v2.0/src/skill",
			want: &gitTreeURL{
				RepoURL: "https://gitlab.com/user/repo",
				Ref:     "v2.0",
				Subpath: "src/skill",
			},
		},
		{
			// /tree/<ref> with no subpath
			url: "https://github.com/user/repo/tree/develop",
			want: &gitTreeURL{
				RepoURL: "https://github.com/user/repo",
				Ref:     "develop",
			},
		},
		{
			// Plain repo URL, not a tree URL
			url:     "https://github.com/user/repo",
			wantNil: true,
		},
		{
			// Non-GitHub/GitLab host
			url:     "https://example.com/user/repo/tree/main/path",
			wantNil: true,
		},
		{
			url:     "/local/path",
			wantNil: true,
		},
	}
	for _, tt := range tests {
		got := parseGitTreeURL(tt.url)
		if tt.wantNil {
			if got != nil {
				t.Errorf("parseGitTreeURL(%q) = %+v, want nil", tt.url, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseGitTreeURL(%q) = nil, want %+v", tt.url, tt.want)
			continue
		}
		if got.RepoURL != tt.want.RepoURL || got.Ref != tt.want.Ref || got.Subpath != tt.want.Subpath {
			t.Errorf("parseGitTreeURL(%q) = %+v, want %+v", tt.url, got, tt.want)
		}
	}
}

func createTarGz(t *testing.T, archivePath, srcRoot, skillName string) {
	t.Helper()
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	skillDir := filepath.Join(srcRoot, skillName)
	filepath.Walk(skillDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(srcRoot, path)
		header, _ := tar.FileInfoHeader(info, "")
		header.Name = rel
		tw.WriteHeader(header)
		if !info.IsDir() {
			data, _ := os.ReadFile(path)
			tw.Write(data)
		}
		return nil
	})
}

func TestLoadCatalogDiscoversSkills(t *testing.T) {
	root := t.TempDir()
	createTestSkill(t, root, "test-skill", "a test skill")

	catalog, err := LoadCatalog([]string{root})
	if err != nil {
		t.Fatalf("LoadCatalog() error = %v", err)
	}
	if catalog == nil {
		t.Fatal("LoadCatalog() returned nil, want catalog")
	}
	if _, ok := catalog.Get("test-skill"); !ok {
		t.Fatal("catalog missing test-skill")
	}
}
