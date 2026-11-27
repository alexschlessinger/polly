package control

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// The transcript line of the control contract is a message as the backend's
// store keeps it: its fields typed where polly keeps them in metadata (usage,
// a stream error, a tool's outcome, a composed prompt), and its own metadata
// the provider's replay state. These convert between that line and polly's
// messages.ChatMessage, each way, without losing what either keeps.

// Line is a transcript line as the backend writes it.
type Line struct {
	Role           string         `json:"role"`
	Content        string         `json:"content,omitempty"`
	Parts          []LinePart     `json:"parts,omitempty"`
	TextBlocks     []LineBlock    `json:"text_blocks,omitempty"`
	ToolCalls      []LineToolCall `json:"tool_calls,omitempty"`
	ToolCallID     string         `json:"tool_call_id,omitempty"`
	ToolName       string         `json:"tool_name,omitempty"`
	Reasoning      string         `json:"reasoning,omitempty"`
	StopReason     string         `json:"stop_reason,omitempty"`
	Usage          *LineUsage     `json:"usage,omitempty"`
	ErrorText      *string        `json:"error_text,omitempty"`
	ToolSucceeded  *bool          `json:"tool_succeeded,omitempty"`
	AgentSynthetic bool           `json:"agent_synthetic,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// LinePart is one part of a multimodal line.
type LinePart struct {
	Kind      string `json:"kind"` // text, image_url, image_base64, image_file
	Text      string `json:"text,omitempty"`
	ImageURL  string `json:"image_url,omitempty"`
	ImageData string `json:"image_data,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	FileName  string `json:"file_name,omitempty"`
	// Path and Bytes are an image_file's: where the backend saved the image
	// (among the session's outputs) and its size.
	Path  string `json:"path,omitempty"`
	Bytes uint64 `json:"bytes,omitempty"`
}

// LineBlock is one phase-aware assistant text block.
type LineBlock struct {
	ID    string `json:"id,omitempty"`
	Phase string `json:"phase,omitempty"`
	Text  string `json:"text,omitempty"`
}

// LineToolCall is a tool call of an assistant line.
type LineToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	DisplayID string `json:"display_id,omitempty"`
}

// OutputRefs makes the artifact reference an image the backend saved among a
// session's outputs is read back by: the reference names the output, and the
// artifact store that made it reads the output by it.
type OutputRefs interface {
	OutputRef(output string, bytes int64, mimeType, fileName string) artifacts.Ref
}

// The metadata keys polly derives from a line's typed fields: they go back
// to the fields, never into the line's own metadata.
var derivedKeys = []string{
	messages.MetadataKeyInputTokens,
	messages.MetadataKeyOutputTokens,
	messages.MetadataKeyCacheReadInputTokens,
	messages.MetadataKeyCacheWriteInputTokens,
	messages.MetadataKeyIsError,
	messages.MetadataKeyError,
	messages.MetadataKeyToolSucceeded,
	messages.MetadataKeyAgentSynthetic,
	metadataKeyTokensReported,
	metadataKeyCacheReported,
}

// Usage that was reported as zero counts is told apart from none reported.
const (
	metadataKeyTokensReported = "backend_tokens_reported"
	metadataKeyCacheReported  = "backend_cache_reported"
)

// ParseLine reads one transcript line.
func ParseLine(raw json.RawMessage) (Line, error) {
	var line Line
	if err := json.Unmarshal(raw, &line); err != nil {
		return Line{}, fmt.Errorf("read a transcript line: %w", err)
	}
	return line, nil
}

// LineToMessage converts a transcript line of session's to polly's message.
// refs makes the references of images the backend saved; nil keeps them as
// they are, unreadable.
func LineToMessage(raw json.RawMessage, session ID, refs OutputRefs) (messages.ChatMessage, error) {
	line, err := ParseLine(raw)
	if err != nil {
		return messages.ChatMessage{}, err
	}
	return line.Message(session, refs), nil
}

// Message converts the line to polly's message.
func (l Line) Message(session ID, refs OutputRefs) messages.ChatMessage {
	msg := messages.ChatMessage{
		Role:       l.Role,
		Content:    l.Content,
		ToolCallID: l.ToolCallID,
		ToolName:   l.ToolName,
		Reasoning:  l.Reasoning,
		StopReason: messages.StopReason(l.StopReason),
	}
	if len(l.Metadata) > 0 {
		msg.Metadata = maps.Clone(l.Metadata)
	}
	set := func(key string, value any) {
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		msg.Metadata[key] = value
	}
	for _, b := range l.TextBlocks {
		msg.TextBlocks = append(msg.TextBlocks, messages.AssistantText{ID: b.ID, Phase: messages.AssistantPhase(b.Phase), Text: b.Text})
	}
	for _, c := range l.ToolCalls {
		args := c.Arguments
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{ID: c.ID, Name: c.Name, Arguments: args, DisplayID: c.DisplayID})
	}
	for _, p := range l.Parts {
		msg.Parts = append(msg.Parts, p.part(session, refs))
	}
	if u := l.Usage; u != nil {
		if u.TokensReported {
			set(messages.MetadataKeyInputTokens, float64(u.InputTokens))
			set(messages.MetadataKeyOutputTokens, float64(u.OutputTokens))
			set(metadataKeyTokensReported, true)
		}
		if u.CacheReported {
			set(messages.MetadataKeyCacheReadInputTokens, float64(u.CacheReadTokens))
			set(messages.MetadataKeyCacheWriteInputTokens, float64(u.CacheWriteTokens))
			set(metadataKeyCacheReported, true)
		}
	}
	if l.ErrorText != nil {
		set(messages.MetadataKeyIsError, true)
		set(messages.MetadataKeyError, *l.ErrorText)
	}
	if l.ToolSucceeded != nil {
		set(messages.MetadataKeyToolSucceeded, *l.ToolSucceeded)
	}
	if l.AgentSynthetic {
		set(messages.MetadataKeyAgentSynthetic, true)
	}
	return msg
}

func (p LinePart) part(session ID, refs OutputRefs) messages.ContentPart {
	switch p.Kind {
	case "text":
		return messages.ContentPart{Type: "text", Text: p.Text, FileName: p.FileName, MimeType: p.MimeType}
	case "image_url":
		return messages.ContentPart{Type: "image_url", ImageURL: p.ImageURL, MimeType: p.MimeType, FileName: p.FileName}
	case "image_base64":
		return messages.ContentPart{Type: "image_base64", ImageData: p.ImageData, MimeType: p.MimeType, FileName: p.FileName}
	case "image_file":
		name := filepath.Base(p.Path)
		part := messages.ContentPart{Type: "image_artifact", MimeType: p.MimeType, FileName: p.FileName}
		if refs != nil && name != "" && name != "." && name != "/" {
			ref := refs.OutputRef(name, int64(p.Bytes), p.MimeType, p.FileName)
			part.Artifact = &ref
			part.Reference = ref.ImageToken
		}
		return part
	}
	return messages.ContentPart{Type: p.Kind, Text: p.Text}
}

// ImageReader reads the bytes of an image artifact for a line that carries
// it inline.
type ImageReader func(ref *artifacts.Ref) ([]byte, error)

// MessageToLine converts polly's message to a transcript line, as a prompt's
// content carries it. read inlines image artifacts; nil refuses them.
func MessageToLine(msg messages.ChatMessage, read ImageReader) (json.RawMessage, error) {
	line, err := LineOf(msg, read)
	if err != nil {
		return nil, err
	}
	return json.Marshal(line)
}

// PromptLine converts a user's message to the line a prompt carries: as
// MessageToLine, but a message of plain text parts alone crosses as its
// content, as the backend keeps a prompt typed as text.
func PromptLine(msg messages.ChatMessage, read ImageReader) (json.RawMessage, error) {
	if msg.Content == "" && len(msg.Parts) > 0 {
		plain := true
		var text strings.Builder
		for _, p := range msg.Parts {
			if p.Type != "text" || p.FileName != "" || p.MimeType != "" {
				plain = false
				break
			}
			text.WriteString(p.Text)
		}
		if plain {
			msg = msg.Clone()
			msg.Content, msg.Parts = text.String(), nil
		}
	}
	return MessageToLine(msg, read)
}

// LineOf converts polly's message to the line's typed form.
func LineOf(msg messages.ChatMessage, read ImageReader) (Line, error) {
	line := Line{
		Role:       msg.Role,
		Content:    msg.Content,
		ToolCallID: msg.ToolCallID,
		ToolName:   msg.ToolName,
		Reasoning:  msg.Reasoning,
		StopReason: string(msg.StopReason),
	}
	if line.Role == "" {
		line.Role = messages.MessageRoleUser
	}
	for _, b := range msg.TextBlocks {
		line.TextBlocks = append(line.TextBlocks, LineBlock{ID: b.ID, Phase: string(b.Phase), Text: b.Text})
	}
	for _, c := range msg.ToolCalls {
		args := c.Arguments
		if args == "{}" {
			args = ""
		}
		line.ToolCalls = append(line.ToolCalls, LineToolCall{ID: c.ID, Name: c.Name, Arguments: args, DisplayID: c.DisplayID})
	}
	for _, p := range msg.Parts {
		lp, err := linePart(p, read)
		if err != nil {
			return Line{}, err
		}
		line.Parts = append(line.Parts, lp)
	}
	md := msg.Metadata
	if tokens, _ := md[metadataKeyTokensReported].(bool); tokens || md[messages.MetadataKeyInputTokens] != nil || md[messages.MetadataKeyOutputTokens] != nil {
		line.Usage = &LineUsage{InputTokens: uint64(msg.GetInputTokens()), OutputTokens: uint64(msg.GetOutputTokens()), TokensReported: true}
	}
	if cache, _ := md[metadataKeyCacheReported].(bool); cache || md[messages.MetadataKeyCacheReadInputTokens] != nil || md[messages.MetadataKeyCacheWriteInputTokens] != nil {
		if line.Usage == nil {
			line.Usage = &LineUsage{}
		}
		line.Usage.CacheReadTokens = uint64(msg.GetCacheReadInputTokens())
		line.Usage.CacheWriteTokens = uint64(msg.GetCacheWriteInputTokens())
		line.Usage.CacheReported = true
	}
	if isErr, _ := md[messages.MetadataKeyIsError].(bool); isErr {
		text, _ := md[messages.MetadataKeyError].(string)
		line.ErrorText = &text
	}
	if ok, present := md[messages.MetadataKeyToolSucceeded].(bool); present {
		line.ToolSucceeded = &ok
	}
	line.AgentSynthetic, _ = md[messages.MetadataKeyAgentSynthetic].(bool)
	if len(md) > 0 {
		rest := maps.Clone(md)
		for _, key := range derivedKeys {
			delete(rest, key)
		}
		if len(rest) > 0 {
			line.Metadata = rest
		}
	}
	return line, nil
}

func linePart(p messages.ContentPart, read ImageReader) (LinePart, error) {
	switch p.Type {
	case "text":
		return LinePart{Kind: "text", Text: p.Text, FileName: p.FileName, MimeType: p.MimeType}, nil
	case "image_url":
		return LinePart{Kind: "image_url", ImageURL: p.ImageURL, MimeType: p.MimeType, FileName: p.FileName}, nil
	case "image_base64":
		return LinePart{Kind: "image_base64", ImageData: p.ImageData, MimeType: p.MimeType, FileName: p.FileName}, nil
	case "image_artifact":
		if read == nil || p.Artifact == nil {
			return LinePart{}, fmt.Errorf("image %s is not readable here", p.Reference)
		}
		data, err := read(p.Artifact)
		if err != nil {
			return LinePart{}, fmt.Errorf("read image %s: %w", p.Reference, err)
		}
		mime := p.MimeType
		if mime == "" {
			mime = p.Artifact.MIMEType
		}
		return LinePart{Kind: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data), MimeType: mime, FileName: p.FileName}, nil
	}
	return LinePart{}, fmt.Errorf("a %s part cannot cross to the backend", p.Type)
}
