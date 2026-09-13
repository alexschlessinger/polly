package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

const composerMetadataKey = "polly_composer_v1"
const maxContextFiles = 32
const maxContextTextBytes = 256 << 10
const maxContextTotalBytes = 1 << 20
const maxContextImageBytes = 32 << 20

type composerReference struct {
	start, end int
	kind, name string
}
type composerFileBinding struct {
	Reference string `json:"reference"`
	Part      int    `json:"part"`
}
type composerMetadata struct {
	Version   int                   `json:"version"`
	Draft     string                `json:"draft"`
	Skills    []string              `json:"skills,omitempty"`
	Activated bool                  `json:"activated,omitempty"`
	Files     []composerFileBinding `json:"files,omitempty"`
}

// scanComposerReferences uses rune offsets, matching the editor. Code spans,
// fences, escaped sigils and tokens without a name remain literal.
func scanComposerReferences(text string) []composerReference {
	tokens := scanComposerReferenceTokens(text)
	refs := tokens[:0]
	for _, token := range tokens {
		if token.name != "" {
			refs = append(refs, token)
		}
	}
	return refs
}

// Completion also needs unfinished tokens such as @, / and /skill . They do
// not become submitted references until a name is supplied.
func scanComposerReferenceTokens(text string) []composerReference {
	rs := []rune(text)
	var refs []composerReference
	code := 0
	for i := 0; i < len(rs); {
		if rs[i] == '\\' && i+1 < len(rs) {
			i += 2
			continue
		}
		if rs[i] == '`' || (rs[i] == '~' && i+2 < len(rs) && rs[i+1] == '~' && rs[i+2] == '~') {
			ch := rs[i]
			n := 1
			for i+n < len(rs) && rs[i+n] == ch {
				n++
			}
			mark := n
			if ch == '~' {
				mark = -n
			}
			if code == 0 {
				code = mark
			} else if code == mark {
				code = 0
			}
			i += n
			continue
		}
		if code != 0 || (rs[i] != '@' && rs[i] != '/') || (i > 0 && !unicode.IsSpace(rs[i-1]) && rs[i-1] != '(') {
			i++
			continue
		}
		start := i
		kind := string(rs[i])
		i++
		var name string
		if kind == "@" && i < len(rs) && (rs[i] == '"' || rs[i] == '\'') {
			quote := rs[i]
			i++
			begin := i
			for i < len(rs) {
				if rs[i] == '\\' && quote == '"' && i+1 < len(rs) {
					i += 2
					continue
				}
				if rs[i] == quote {
					break
				}
				i++
			}
			if i < len(rs) {
				name = string(rs[begin:i])
				i++
				if quote == '"' {
					if decoded, err := strconv.Unquote(string(rs[begin-1 : i])); err == nil {
						name = decoded
					}
				}
			}

		} else {
			begin := i
			for i < len(rs) && !unicode.IsSpace(rs[i]) && rs[i] != ')' && rs[i] != '`' {
				i++
			}
			name = string(rs[begin:i])
		}
		if kind == "/" && name == "skill" {
			for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
				i++
			}
			begin := i
			for i < len(rs) && (unicode.IsLetter(rs[i]) || unicode.IsDigit(rs[i]) || rs[i] == '-') {
				i++
			}
			name = string(rs[begin:i])
			kind = "skill"
		}
		refs = append(refs, composerReference{start, i, kind, name})
	}
	return refs
}

func fileReference(path string) string {
	if strings.ContainsAny(path, " \t\n\r\"'()") {
		return "@" + strconv.Quote(path)
	}
	return "@" + path
}

func readComposerMetadata(msg messages.ChatMessage) (composerMetadata, bool) {
	var md composerMetadata
	v, ok := msg.Metadata[composerMetadataKey]
	if !ok {
		return md, false
	}
	data, err := json.Marshal(v)
	if err != nil || json.Unmarshal(data, &md) != nil || md.Version != 1 {
		return composerMetadata{}, false
	}
	return md, true
}
func writeComposerMetadata(msg *messages.ChatMessage, md composerMetadata) {
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[composerMetadataKey] = md
}
func referenceSkills(refs []composerReference, catalog *skills.Catalog) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		if ref.kind != "/" && ref.kind != "skill" {
			continue
		}
		if ref.kind == "/" {
			if _, reserved := defaultReplCommands.get("/" + ref.name); reserved {
				continue
			}
		}
		_, ok := catalog.Get(ref.name)
		if !ok {
			if ref.kind == "skill" {
				return nil, fmt.Errorf("unknown skill: %s", ref.name)
			}
			continue
		}
		if !seen[ref.name] {
			out = append(out, ref.name)
			seen[ref.name] = true
		}
	}
	return out, nil
}
func leadingSkillReference(prompt string, catalog *skills.Catalog) bool {
	// An unfinished explicit skill reference is literal prompt text, not an
	// unknown slash command. Completion can still offer names while editing.
	if strings.TrimSpace(prompt) == "/skill" {
		return true
	}
	refs := scanComposerReferences(prompt)
	if len(refs) == 0 || refs[0].start != 0 {
		return false
	}
	if refs[0].kind == "skill" {
		return true
	}
	if refs[0].kind != "/" {
		return false
	}
	if _, reserved := defaultReplCommands.get("/" + refs[0].name); reserved {
		return false
	}
	_, ok := catalog.Get(refs[0].name)
	return ok
}

func contextFilePart(ctx context.Context, registry *tools.ToolRegistry, path, reference string) (messages.ContentPart, error) {
	if registry == nil {
		return messages.ContentPart{}, fmt.Errorf("file attachments require a session read policy")
	}
	abs, data, err := registry.ReadContextFile(ctx, path, maxContextImageBytes)
	if err != nil {
		return messages.ContentPart{}, err
	}
	// Recognize images by bytes, not just filename. The existing normalizer
	// remains authoritative for supported image formats and upload limits.
	if isContextImage(data) {
		part, err := prepareImageBytesForUpload(data, filepath.Base(abs))
		if err != nil {
			return messages.ContentPart{}, err
		}
		part.Reference = reference
		return *part, nil
	}
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return messages.ContentPart{}, fmt.Errorf("PDF attachments are not supported")
	}
	if len(data) > maxContextTextBytes {
		return messages.ContentPart{}, fmt.Errorf("%s exceeds text attachment limit (%d bytes)", path, maxContextTextBytes)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return messages.ContentPart{}, fmt.Errorf("%s is not a supported text file or image", path)
	}
	label := fmt.Sprintf("\n\nAttached file %q (%d bytes):\n", abs, len(data))
	return messages.ContentPart{Type: "text", FileName: abs, Reference: reference, Text: label + string(data) + "\nEnd attached file.\n"}, nil
}
func isContextImage(data []byte) bool {
	_, _, err := image.DecodeConfig(bytes.NewReader(data))
	return err == nil
}
func hasComposerReferences(prompt string, catalog *skills.Catalog) bool {
	refs := scanComposerReferences(prompt)
	for _, ref := range refs {
		if ref.kind == "@" || ref.kind == "skill" {
			return true
		}
	}
	names, _ := referenceSkills(refs, catalog)
	return len(names) > 0
}

func prepareComposerReferences(ctx context.Context, prompt, root string, registry *tools.ToolRegistry, catalog *skills.Catalog, base messages.ChatMessage, snapshots map[string]messages.ContentPart) (messages.ChatMessage, error) {
	refs := scanComposerReferences(prompt)
	names, err := referenceSkills(refs, catalog)
	if err != nil {
		return messages.ChatMessage{}, err
	}
	var parts []messages.ContentPart
	seen := map[string]int{}
	var bindings []composerFileBinding
	baseParts := max(1, len(base.Parts))
	total := 0
	rs := []rune(prompt)
	for _, ref := range refs {
		if ref.kind != "@" {
			continue
		}
		token := string(rs[ref.start:ref.end])
		path := ref.name
		if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
			path = filepath.Join(root, path)
		}
		key := filepath.Clean(path)
		if index, ok := seen[key]; ok {
			bindings = append(bindings, composerFileBinding{token, index})
			continue
		}
		seen[key] = baseParts + len(parts)
		bindings = append(bindings, composerFileBinding{token, seen[key]})
		if len(seen)+max(0, len(base.Parts)-1) > maxContextFiles {
			return messages.ChatMessage{}, fmt.Errorf("maximum is %d file attachments", maxContextFiles)
		}
		part, ok := snapshots[token]
		if !ok {
			part, err = contextFilePart(ctx, registry, path, token)
			if err != nil {
				return messages.ChatMessage{}, err
			}
		}
		if part.Type == "text" {
			total += contextTextSize(part)
			if total > maxContextTotalBytes {
				return messages.ChatMessage{}, fmt.Errorf("combined text attachments exceed 1 MiB")
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 && len(names) == 0 {
		return base, nil
	}
	if len(base.Parts) == 0 {
		base.Parts = []messages.ContentPart{{Type: "text", Text: base.Content}}
		base.Content = ""
	}
	base.Parts = append(base.Parts, parts...)
	writeComposerMetadata(&base, composerMetadata{Version: 1, Draft: prompt, Skills: names, Files: bindings})
	return base, messages.ValidateImageMessage(base)
}

// Activation belongs to execution, never acceptance/queueing. Keep each
// successful activation even if a later one fails, matching runtime semantics.
func activateComposerSkills(ctx context.Context, state *conversationState, msg messages.ChatMessage) (messages.ChatMessage, error) {
	md, ok := readComposerMetadata(msg)
	if !ok || len(md.Skills) == 0 {
		return msg, nil
	}
	if state == nil || state.skillRuntime == nil {
		return msg, fmt.Errorf("skill runtime is unavailable")
	}
	var guidance []messages.ContentPart
	for _, name := range md.Skills {
		if err := ctx.Err(); err != nil {
			return msg, err
		}
		result, err := state.skillRuntime.Activate(name)
		persistErr := persistActiveSkills(ctx, state.session, state.skillRuntime, state.skillSources)
		if err != nil {
			return msg, fmt.Errorf("activate skill %s (earlier activations remain active): %w", name, err)
		}
		if persistErr != nil {
			return msg, persistErr
		}
		guidance = append(guidance, messages.ContentPart{Type: "text", FileName: "skill:" + name, Text: "\n\nExplicitly requested skill instructions (activated by Polly):\n" + result + "\n"})
	}
	if !md.Activated {
		msg.Parts = append(msg.Parts, guidance...)
		md.Activated = true
		writeComposerMetadata(&msg, md)
	}
	return msg, nil
}

func contextTextSize(part messages.ContentPart) int {
	marker := " bytes):\n"
	if i := strings.Index(part.Text, marker); i >= 0 && strings.HasPrefix(part.Text, "\n\nAttached file ") {
		return len(strings.TrimSuffix(part.Text[i+len(marker):], "\nEnd attached file.\n"))
	}
	return len(part.Text)
}
