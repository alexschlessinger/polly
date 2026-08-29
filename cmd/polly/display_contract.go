package main

import (
	"github.com/alexschlessinger/pollytool/messages"
)

// The display contract tells the model how its output will be rendered. It is
// composed into the request's system message at send time — per frontend, never
// persisted — so the stored system prompt holds only the user's persona and a
// context moves freely between the pipe, the fallback REPL, and the managed TUI.
const (
	// plainDisplayContract covers one-shot/pipe output and the fallback line
	// REPL, where markdown would be noise on raw stdout.
	plainDisplayContract = "Your output is written to a plain unix terminal or pipe. Be terse. Do not use markdown; plain text only."

	// tuiDisplayContract covers the managed REPL. It must mirror what the
	// renderer in markdown.go actually supports: goldmark parses only
	// strikethrough beyond core markdown (no tables), HTML blocks display as
	// source, fence language tags drive chroma highlighting, and local image
	// references render inline. Flip the tables line if table rendering lands.
	tuiDisplayContract = "Your output is displayed in a terminal TUI that renders markdown. Be terse. Tag code fences with a language for syntax highlighting. Markdown tables and raw HTML are not rendered — use lists instead. Display a local image file inline with ![alt](path). To show markdown source literally, fence it."
)

// legacySystemPromptDefaults are the pre-refactor default system prompts, which
// baked a display contract into the persisted prompt. They are recognized so
// contexts created before the split read back as persona-less instead of
// carrying a stale, possibly wrong-mode contract.
var legacySystemPromptDefaults = []string{
	"Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.",
	"Your output will be displayed in a unix tui. Be terse. Use markdown where it aids readability. Use code blocks where appropriate, including for markdown.",
}

// normalizeLegacySystemPrompt maps either legacy default to the empty persona.
func normalizeLegacySystemPrompt(s string) string {
	for _, legacy := range legacySystemPromptDefaults {
		if s == legacy {
			return ""
		}
	}
	return s
}

// displayContractFor selects the contract for the active frontend. One-shot and
// the fallback line REPL both write plain text; only the managed REPL renders
// markdown.
func displayContractFor(mode conversationMode, managedREPL bool) string {
	if mode == conversationModeREPL && managedREPL {
		return tuiDisplayContract
	}
	return plainDisplayContract
}

// applyDisplayContract merges the contract into the request's system message.
// Providers keep a single system prompt (Anthropic and Gemini take only the
// last system message), so the contract must extend the existing message rather
// than ride alongside it; skill guidance is folded into the same message
// downstream by CompletionRequest.ResolvedMessages. A legacy default seeded
// into old transcripts is replaced outright — it *was* the display contract.
// msgs must be a request-local copy: the first element is edited in place.
func applyDisplayContract(msgs []messages.ChatMessage, contract string) []messages.ChatMessage {
	if contract == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == messages.MessageRoleSystem {
		if persona := normalizeLegacySystemPrompt(msgs[0].Content); persona != "" {
			msgs[0].Content = persona + "\n\n" + contract
		} else {
			msgs[0].Content = contract
		}
		return msgs
	}
	return append([]messages.ChatMessage{{
		Role:    messages.MessageRoleSystem,
		Content: contract,
	}}, msgs...)
}
