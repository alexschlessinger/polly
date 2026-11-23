package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

// A read's output is highlighted as the language of the file it names:
// usually by extension, by whole name for a file known that way, even one
// whose extension an earlier read has already settled.
func TestReadOutputLanguageFollowsTheFileName(t *testing.T) {
	const body = "all: build\n\t@echo \"done\" # 1\n"
	for _, file := range []struct{ path, language string }{
		{"docs/notes.txt", "plaintext"},
		{"CMakeLists.txt", "CMake"},
		{"cmd/polly/main.go", "Go"},
		{"/src/main.go.orig", "Go"},
		{"tools/run.py", "Python"},
		{"Makefile", "Makefile"},
		{"Dockerfile", "Docker"},
		{"deploy/Dockerfile.dev", "Docker"},
		{"LICENSE", ""},
		{"data.unknown", ""},
	} {
		for _, call := range []messages.ChatMessageToolCall{
			{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, file.path)},
			{Name: "read", Arguments: fmt.Sprintf(`{"file_path":%q}`, file.path)},
		} {
			output := newInlineToolOutput(call, messages.ChatMessage{Role: messages.MessageRoleTool, Content: body})
			if output.language != file.language {
				t.Fatalf("%s %s: language = %q, want %q", call.Name, file.path, output.language, file.language)
			}
			if want := formatInlineToolOutput(body, file.language); output.text() != want {
				t.Fatalf("%s %s: text = %q, want %q", call.Name, file.path, output.text(), want)
			}
		}
	}
}

// BenchmarkHydrateReadFileHistory resumes a session that read its way
// through a repository: one turn of a thousand read_file steps, each naming
// a different Go file. Finding each file's lexer was nine tenths of it; a
// CPU profile should show no chroma LexerRegistry.Match at all.
//
//	go test ./cmd/polly -run '^$' -bench '^BenchmarkHydrateReadFileHistory$' -benchtime=10x -benchmem -count=1
func BenchmarkHydrateReadFileHistory(b *testing.B) {
	history := readFileHistory(1000, 1, "package main\n")
	b.ReportAllocs()
	for b.Loop() {
		newReplModel().hydrateHistory(history, "ctx")
	}
}

// BenchmarkHydrateReadFileBodies resumes a session whose reads returned
// whole files: one step of two hundred read_file calls, each a 280-line Go
// file. Replayed rows start closed, so none of those bodies should be
// highlighted: a CPU profile should show no chroma tokenising.
//
//	go test ./cmd/polly -run '^$' -bench '^BenchmarkHydrateReadFileBodies$' -benchtime=10x -benchmem -count=1
func BenchmarkHydrateReadFileBodies(b *testing.B) {
	history := readFileHistory(1, 200, goFileBody)
	b.ReportAllocs()
	for b.Loop() {
		newReplModel().hydrateHistory(history, "ctx")
	}
}

var goFileBody = strings.Repeat("func value(i int) (string, error) {\n\tif i > 0 {\n\t\treturn fmt.Sprintf(\"%d\", i), nil // positive\n\t}\n\treturn \"\", errors.New(\"negative\")\n}\n\n", 40)

// readFileHistory is one turn of read_file steps, calls to a step, each call
// naming a different Go file and returning body.
func readFileHistory(steps, calls int, body string) []messages.ChatMessage {
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "read them"}}
	for step := range steps {
		request := messages.ChatMessage{Role: messages.MessageRoleAssistant}
		for i := range calls {
			n := step*calls + i
			request.ToolCalls = append(request.ToolCalls, messages.ChatMessageToolCall{ID: fmt.Sprint("call-", n), Name: "read_file", Arguments: fmt.Sprintf(`{"path":"f%04d.go"}`, n)})
		}
		history = append(history, request)
		for _, call := range request.ToolCalls {
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: body})
		}
	}
	return append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done"})
}
