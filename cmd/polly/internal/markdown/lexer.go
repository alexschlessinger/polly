package markdown

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

// Chroma finds the lexer for a file name, or for a language it does not know
// by name, by trying every glob of every lexer: about a millisecond a search,
// and a replayed session asks once for each file it read and each such fence.
// lexerMemo remembers the answers, which cannot change: nothing registers a
// lexer after init.
//
// Its keys come from model output, so it is bounded. Full, it starts over; a
// key longer than lexerMemoKeyBytes is searched for every time.
var lexerMemo struct {
	sync.Mutex
	found map[lexerKey]chroma.Lexer
}

const (
	lexerMemoKeys     = 1024
	lexerMemoKeyBytes = 128
)

type lexerKey struct {
	kind lexerKeyKind
	text string
}

type lexerKeyKind byte

const (
	keyLanguage lexerKeyKind = iota // lexers.Get(text)
	keyFileName                     // lexers.Match(text)
	keyFileTail                     // lexers.Match of any name fileKey gives this tail
)

// memoLexer is find(arg), remembered under key.
func memoLexer(key lexerKey, find func(string) chroma.Lexer, arg string) chroma.Lexer {
	if len(key.text) > lexerMemoKeyBytes {
		return find(arg)
	}
	lexerMemo.Lock()
	lexer, ok := lexerMemo.found[key]
	lexerMemo.Unlock()
	if ok {
		return lexer
	}
	// Unlocked: the search is the slow part, and callers that race to it
	// find the same lexer.
	lexer = find(arg)
	key.text = strings.Clone(key.text)
	lexerMemo.Lock()
	defer lexerMemo.Unlock()
	if lexerMemo.found == nil || len(lexerMemo.found) >= lexerMemoKeys {
		lexerMemo.found = make(map[lexerKey]chroma.Lexer)
	}
	lexerMemo.found[key] = lexer
	return lexer
}

// lexerFor is lexers.Get: the lexer for a language name, alias or extension,
// nil when chroma has none.
func lexerFor(lang string) chroma.Lexer {
	return memoLexer(lexerKey{keyLanguage, lang}, lexers.Get, lang)
}

// FileLanguage names the language HighlightCodeLines should read a file as:
// the lexer chroma matches to its name, or "" when none claims it.
func FileLanguage(path string) string {
	// Chroma matches the base name, and takes it from the path itself.
	if lexer := memoLexer(fileKey(filepath.Base(path)), lexers.Match, path); lexer != nil {
		return lexer.Config().Name
	}
	return ""
}

// fileKey is the memo key for a file's base name. Names with equal keys match
// the same lexer, so one search answers for all of them.
//
// Nearly every lexer glob is "*.ext". What follows its star has to start at
// one of the name's dots, so such a glob sees only the name from its first
// dot on, and names that share that tail ("a.go" and "b.go", "a.tar.gz" and
// "b.tar.gz") share an answer. Every other glob spells out how the name
// starts ("Makefile", "Dockerfile.*", "bash_*"), and a name that starts like
// one of them is keyed whole. Chroma also tries each glob with a backup
// suffix appended ("*.go.bak", "Makefile.orig"), which moves no glob from one
// kind to the other.
func fileKey(name string) lexerKey {
	for _, start := range namedFileStarts() {
		if strings.HasPrefix(name, start) {
			return lexerKey{keyFileName, name}
		}
	}
	tail := ""
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		tail = name[dot:]
	}
	return lexerKey{keyFileTail, tail}
}

// namedFileStarts lists the literal text each lexer glob that is not "*.ext"
// starts with. A glob that starts with a wildcard of its own contributes "",
// which keys every name whole: slower, never wrong.
var namedFileStarts = sync.OnceValue(func() []string {
	var starts []string
	for _, lexer := range lexers.GlobalLexerRegistry.Lexers {
		config := lexer.Config()
		for _, globs := range [][]string{config.Filenames, config.AliasFilenames} {
			for _, glob := range globs {
				if ext, ok := strings.CutPrefix(glob, "*."); ok && !strings.Contains(ext, "*") {
					continue
				}
				start := glob
				if meta := strings.IndexAny(glob, `*?[\`); meta >= 0 {
					start = glob[:meta]
				}
				starts = append(starts, start)
			}
		}
	}
	return starts
})
