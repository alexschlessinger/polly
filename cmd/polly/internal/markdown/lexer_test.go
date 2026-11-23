package markdown

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// chromaLanguage is the search FileLanguage remembers, made every time.
func chromaLanguage(path string) string {
	if lexer := lexers.Match(path); lexer != nil {
		return lexer.Config().Name
	}
	return ""
}

// forgetLexers empties the memo for a test, and again once it is over.
func forgetLexers(t *testing.T) {
	forget := func() {
		lexerMemo.Lock()
		lexerMemo.found = nil
		lexerMemo.Unlock()
	}
	forget()
	t.Cleanup(forget)
}

// One remembered answer serves every name with its key, so the order files
// are read in must not matter: an extension read first cannot answer for a
// file known by its whole name, nor that file for the extension.
func TestFileLanguageMatchesChroma(t *testing.T) {
	paths := []string{
		"notes.txt", "build/CMakeLists.txt", "meson_options.txt", "requirements.txt",
		"main.go", "tools/bash_test.go", "bash.go", "go.mod", "v.mod",
		"Makefile", "Makefile.am", "Makefile.go", "makefile", "GNUmakefile", "Makefiles.go",
		"Dockerfile", "Dockerfile.dev", "docker/Dockerfile.go", "api.Dockerfile", "xDockerfile.go",
		".env", ".env.local", ".envrc", "x.env", ".bashrc", "x.bashrc", ".bash_profile",
		"main.go.bak", "main.go~", "main.go.orig.bak", "notes.bak", "Makefile.orig", "lib.rs.in", "config.h.in",
		"a.tar.gz", "index.html.erb", "index.php5", "polly.1", "nginx.conf", "app.conf",
		"LICENSE", "README", "BUILD", "BUILDING.md", "x.md",
		"MAIN.GO", "x.Go", ".go", "x.", "x..go", "a b.py", "naïve.py", "*.go", "[x].go",
		"", ".", "..", "/", "dir/", "dir.d/noext", "dir/c:Makefile", "x." + strings.Repeat("long", lexerMemoKeyBytes),
	}
	want := make([]string, len(paths))
	for i, path := range paths {
		want[i] = chromaLanguage(path)
	}
	for _, backwards := range []bool{false, true} {
		forgetLexers(t)
		for i := range paths {
			if backwards {
				i = len(paths) - 1 - i
			}
			// Twice: the search, then its memory.
			for range 2 {
				if got := FileLanguage(paths[i]); got != want[i] {
					t.Fatalf("FileLanguage(%q) = %q, chroma matches %q (backwards=%v)", paths[i], got, want[i], backwards)
				}
			}
		}
	}
}

// checkFileLanguage holds a name that shares its key to chroma's own answer
// for it, once the plainest name with that key has been remembered: were
// chroma to match the two differently, one would get the other's lexer.
func checkFileLanguage(t *testing.T, path string) {
	t.Helper()
	key := fileKey(filepath.Base(path))
	if key.kind != keyFileTail {
		return // searched for by its whole name: it shares with nothing
	}
	plain := "x" + key.text
	if fileKey(plain) != key {
		t.Fatalf("%q no longer stands for the key of %q; pick another stem", plain, path)
	}
	FileLanguage(plain)
	if got, want := FileLanguage(path), chromaLanguage(path); got != want {
		t.Fatalf("FileLanguage(%q) = %q after %q, chroma matches %q", path, got, plain, want)
	}
}

// globSample is the shortest name a lexer glob matches: nothing for a star,
// the first choice of a class.
func globSample(t *testing.T, glob string) string {
	var sample strings.Builder
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
		case '?':
			sample.WriteByte('x')
		case '[':
			sample.WriteByte(glob[i+1])
			i += strings.IndexByte(glob[i:], ']')
		case '\\':
			i++
			sample.WriteByte(glob[i])
		default:
			sample.WriteByte(glob[i])
		}
	}
	if ok, err := filepath.Match(glob, sample.String()); !ok || err != nil {
		t.Fatalf("sample %q does not match its glob %q: %v", sample.String(), glob, err)
	}
	return sample.String()
}

// Names built from the globs chroma ships. Every file it knows by name
// appears as itself, as a backup and with an extension; then those names cut
// short, extended and recased stand in front of the tails of its extension
// globs, with and without the backup suffixes it ignores.
func TestFileLanguageGeneratedNamesMatchChroma(t *testing.T) {
	var named, tails []string
	for _, lexer := range lexers.GlobalLexerRegistry.Lexers {
		config := lexer.Config()
		for _, globs := range [][]string{config.Filenames, config.AliasFilenames} {
			for _, glob := range globs {
				if sample := globSample(t, glob); strings.HasPrefix(glob, "*") {
					tails = append(tails, sample)
				} else {
					named = append(named, sample)
				}
			}
		}
	}
	backups := []string{"~", ".bak", ".orig", ".in", ".dpkg-dist", ".rpmnew"}
	rng := rand.New(rand.NewSource(1))
	pick := func(from []string) string { return from[rng.Intn(len(from))] }
	for _, name := range named {
		checkFileLanguage(t, name)
		checkFileLanguage(t, name+pick(backups))
		checkFileLanguage(t, name+pick(tails))
	}
	starts := append([]string{"", "x", "f0001", "Notes"}, named...)
	for range 150 {
		name := pick(starts)
		switch rng.Intn(5) {
		case 0:
			name = "x" + name
		case 1:
			name = name[:max(0, len(name)-1)]
		case 2:
			name = strings.ToUpper(name)
		case 3:
			name = strings.ToLower(name)
		}
		for range rng.Intn(3) {
			name += pick(tails)
		}
		if rng.Intn(2) == 0 {
			name += pick(backups)
		}
		checkFileLanguage(t, name)
	}
}

func FuzzFileLanguageMatchesChroma(f *testing.F) {
	for _, name := range []string{"main.go", "Makefile", "bash_test.go", "Dockerfile.dev", "x.go.bak", ".env.local", "a.tar.gz", "x.php5", "LICENSE", ""} {
		f.Add(name)
	}
	f.Fuzz(checkFileLanguage)
}

// A session's reads share a handful of extensions: however many files carry
// one, and whoever asks, it is searched for once.
func TestFileLanguageSearchesOncePerExtension(t *testing.T) {
	forgetLexers(t)
	var readers sync.WaitGroup
	for reader := range 4 {
		readers.Go(func() {
			for i := range 1000 {
				if got := FileLanguage(fmt.Sprintf("pkg%d/f%04d.go", reader, i)); got != "Go" {
					t.Errorf("FileLanguage = %q, want Go", got)
					return
				}
			}
		})
	}
	readers.Wait()
	if len(lexerMemo.found) != 1 {
		t.Fatalf("remembered %d searches for one extension", len(lexerMemo.found))
	}
}

// The keys are whatever a model wrote, so neither their number nor their
// size may grow the memo without limit.
func TestLexerMemoIsBounded(t *testing.T) {
	forgetLexers(t)
	searches := 0
	search := func(string) chroma.Lexer { searches++; return nil }
	for i := range 3 * lexerMemoKeys {
		memoLexer(lexerKey{keyLanguage, fmt.Sprint(i)}, search, "")
		if len(lexerMemo.found) > lexerMemoKeys {
			t.Fatalf("memo holds %d keys, limit %d", len(lexerMemo.found), lexerMemoKeys)
		}
	}
	for _, lookup := range []struct {
		text     string
		searches int
	}{
		{"remembered", 1},
		{strings.Repeat("x", lexerMemoKeyBytes+1), 2},
	} {
		searches = 0
		for range 2 {
			memoLexer(lexerKey{keyLanguage, lookup.text}, search, "")
		}
		if searches != lookup.searches {
			t.Fatalf("two lookups of a %d-byte key searched %d times, want %d", len(lookup.text), searches, lookup.searches)
		}
	}
}

// A fence names its language by name, alias or extension, or names nothing
// chroma knows; remembering the lexer changes none of them. A language and a
// file may share their text ("Makefile.go" is Go to one search and a Makefile
// to the other) without sharing an answer.
func TestLexerForIsChromaGet(t *testing.T) {
	forgetLexers(t)
	for _, lang := range []string{"go", "Go", "GOLANG", "py", "jsonc", "mermaid", "no such language", "Makefile", "Makefile.go", "bash_test.go"} {
		for range 2 {
			if got, want := FileLanguage(lang), chromaLanguage(lang); got != want {
				t.Fatalf("FileLanguage(%q) = %q, chroma matches %q", lang, got, want)
			}
			if got, want := lexerFor(lang), lexers.Get(lang); got != want {
				t.Fatalf("lexerFor(%q) = %v, lexers.Get = %v", lang, got, want)
			}
		}
	}
}
