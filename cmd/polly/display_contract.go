package main

import (
	"github.com/alexschlessinger/pollytool/messages"
)

// The display contract tells the model how its output will be rendered. It is
// composed into the request's system message at send time — per output
// capability, never persisted — so the stored system prompt holds only the
// user's persona and a context moves freely between the pipe, fallback REPL,
// and managed TUI.
const (
	// markdownDisplayContract covers every human-facing frontend. Markdown is
	// an available content format, not a requirement: raw line output preserves
	// the source while rich terminal surfaces render it.
	markdownDisplayContract = "Follow the user's formatting requests. Be concise without losing necessary detail. Markdown is optional; use language-tagged code fences, fences for literal Markdown, and short table cells so rows fit the terminal. Inspect supplied image paths or URLs with view_image."

	// localImageDisplayContract is added only for surfaces that render
	// Markdown: HTML shown as source, tool-image receipts, and local image
	// references drawn as native thumbnails or caption/path fallbacks.
	localImageDisplayContract = "Raw HTML is shown as source. Attached tool images already show an inspection receipt; re-embed one only when it belongs in the answer or the user asks to see it. Embed images with ![alt](path-or-url); this terminal shows thumbnails or captions."

	// richTerminalDisplayContract is markdownDisplayContract plus the
	// rendered-surface sentence. Its HTML, table, and thumbnail claims must
	// track what the managed REPL and the rich line renderer in markdown.go
	// actually do.
	richTerminalDisplayContract = markdownDisplayContract + "\n\n" + localImageDisplayContract

	// contextMechanicsContract teaches the proactive habits the projection's
	// in-band forms cannot: receipts, stubs, and the omission marker explain
	// themselves at the point of use, but the model must know before a turn
	// ends that its reply outlives tool output, and must reach for recall
	// tools instead of re-running work or re-asking the user. Constant bytes
	// on purpose — it rides the stable request prefix. Standing guidance
	// belongs here, composed at send time; the durable receipt and stub forms
	// are byte-stability contracts with persisted history and must not absorb
	// wording changes.
	contextMechanicsContract = "Context is trimmed; the full transcript remains recoverable. Replies persist verbatim, while large tool outputs become artifact receipts. Briefly preserve important findings and decisions in replies. Recover earlier work with read_transcript, read_artifact, or list_artifacts before repeating investigations or questions. Earlier file contents, command output, and test results may be stale: re-read and re-run before editing or claiming validation. To show a stored image again call read_artifact with its artifact ID; writing its [image ...] reference in a reply does not attach it. Discuss context limits only if asked."
)

// sendTimeContracts joins the per-frontend display contract with the
// frontend-independent context-mechanics contract.
func sendTimeContracts(displayContract string) string {
	if displayContract == "" {
		return contextMechanicsContract
	}
	return displayContract + "\n\n" + contextMechanicsContract
}

// displayContractFor describes what the active output surface can interpret.
// Raw output still accepts Markdown — Polly simply preserves the source.
func displayContractFor(capabilities outputCapabilities) string {
	if capabilities.interpretsLocalImages() {
		return richTerminalDisplayContract
	}
	return markdownDisplayContract
}

// applyDisplayContract merges the contract into the request's system message.
// Providers keep a single system prompt (Anthropic and Gemini take only the
// last system message), so the contract must extend the existing message rather
// than ride alongside it; skill guidance is folded into the same message
// downstream by CompletionRequest.ResolvedMessages. (Pre-contract default
// prompts seeded into old transcripts are stripped by the session store's
// schema v2 migration.)
// msgs must be a request-local copy: the first element is edited in place.
func applyDisplayContract(msgs []messages.ChatMessage, contract string) []messages.ChatMessage {
	if contract == "" {
		return msgs
	}
	if len(msgs) > 0 && msgs[0].Role == messages.MessageRoleSystem {
		if persona := msgs[0].Content; persona != "" {
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
