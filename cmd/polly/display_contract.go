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
	markdownDisplayContract = "Your output is emitted as Markdown source or rendered Markdown depending on the terminal. Markdown is supported but optional; follow explicit user formatting requests. Be terse. Tag code fences with a language for syntax highlighting. Raw HTML is not rendered on rich terminal surfaces. Markdown tables render as aligned monospace columns; keep cells short so rows fit the terminal. To show Markdown source literally, fence it. When the user gives you an image path or image URL, attach it with the view_image tool so you can actually see it. Typed images attached by tools produce a user-visible inspection receipt; the TUI groups them under Images viewed."

	// localImageDisplayContract is added only for surfaces that interpret local
	// Markdown images. Native-capable terminals draw a thumbnail; other rich
	// terminals retain the caption/path fallback.
	localImageDisplayContract = "This terminal surface interprets local Markdown image references. Typed tool images already produce a user-visible inspection receipt, so do not repeat a view_image result in your reply unless the image belongs in the final answer or the user explicitly asks to see it there. To deliberately place an image in the reply, embed it as ![alt](path) (or ![alt](url)). Local images render as native thumbnails when supported and as compact captions otherwise."

	// richTerminalDisplayContract must mirror what the managed REPL and rich
	// line renderer in markdown.go actually support: strikethrough and tables
	// beyond core markdown, HTML blocks displayed as source, fence language
	// tags driving chroma highlighting, and local image references rendered
	// inline.
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
	contextMechanicsContract = "Conversation memory: your visible context is a trimmed view of a complete, durable transcript. Your reply text persists across turns verbatim; large tool outputs shrink to artifact receipts once a turn completes, and the oldest exchanges are eventually omitted. Put load-bearing findings in your replies. Everything trimmed stays recoverable — read_artifact reads a stored output by the ID its receipt names, read_transcript searches or pages the full conversation, list_artifacts catalogs stored items — so prefer those tools over re-running work or asking the user to repeat anything. To see a stored image again, call read_artifact with its artifact ID; writing an image token in a reply does not attach it. Handle context limits silently; don't mention them unless the user asks."
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
