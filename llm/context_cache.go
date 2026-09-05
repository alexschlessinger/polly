package llm

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// projectionCache belongs to one Run. Its message prefix is immutable until
// Agent applies a durable spill; that operation invalidates the prefix caches.
// Image bytes are immutable independently of transcript replacements.
type projectionCache struct {
	messageTokens []int
	demotions     map[int]*toolDemotion
	birthForms    map[int]cachedToolForm
	images        map[string]cachedProjectionImage
	imageBytes    int
}

type cachedProjectionImage struct {
	bytes   int64
	encoded string
}

type cachedToolForm struct {
	ref     artifacts.Ref
	content string
}

func (c *projectionCache) invalidateMessages() {
	c.messageTokens = nil
	c.demotions = nil
	c.birthForms = nil
}

func (c *projectionCache) estimates(history []messages.ChatMessage) []int {
	if len(history) < len(c.messageTokens) {
		c.invalidateMessages()
	}
	for _, msg := range history[len(c.messageTokens):] {
		c.messageTokens = append(c.messageTokens, estimateProjectedMessageTokens(msg))
	}
	return c.messageTokens
}

// projectionTokens carries exact estimates through transformations. Only
// messages whose token-bearing fields change need another estimate.
type projectionTokens struct {
	counts []int
	total  int
}

func (p *projectionTokens) update(i int, msg messages.ChatMessage) {
	n := estimateProjectedMessageTokens(msg)
	p.total += n - p.counts[i]
	p.counts[i] = n
}

func (p *projectionTokens) replaceContent(i int, old, content string) {
	delta := estimatedStringTokens(content) - estimatedStringTokens(old)
	p.counts[i] += delta
	p.total += delta
}

func (p *projectionTokens) append(msg messages.ChatMessage) {
	n := estimateProjectedMessageTokens(msg)
	p.counts = append(p.counts, n)
	p.total += n
}

func (p *projectionTokens) omit(history []messages.ChatMessage, start, end int) {
	out := p.counts[:start]
	for i := start; i < end; i++ {
		if history[i].Role == messages.MessageRoleSystem {
			out = append(out, p.counts[i])
		} else {
			p.total -= p.counts[i]
		}
	}
	p.counts = append(out, p.counts[end:]...)
}

// Marker insertion changes one content field, or introduces one new system
// message. Price its byte length directly, preserving per-field rounding.
func projectionMarkerTokenDelta(history []messages.ChatMessage, marker string) (int, int) {
	for i, msg := range history {
		if msg.Role == messages.MessageRoleSystem {
			n := len(marker)
			if msg.Content != "" {
				n += len(msg.Content) + 2
			}
			return i, (n+3)/4 - estimatedStringTokens(msg.Content)
		}
	}
	return -1, 4 + estimatedStringTokens(marker)
}

func (p *projectionTokens) addMarker(history []messages.ChatMessage, marker string) {
	i, delta := projectionMarkerTokenDelta(history, marker)
	if i < 0 {
		p.counts = append([]int{delta}, p.counts...)
	} else {
		p.counts[i] += delta
	}
	p.total += delta
}

type toolDemotion struct {
	content string
	tokens  int
	inline  bool
	ok      bool
	stored  *artifacts.Ref
}

const prospectiveArtifactID = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func prospectiveTextRef(content string) artifacts.Ref {
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	return artifacts.Ref{ID: prospectiveArtifactID, Bytes: int64(len(content)), Lines: lines}
}

// Planning never copies or hashes an inline payload. A digest's spelling does
// not affect receipt length; line counting is done once per immutable message.
func planToolDemotion(msg messages.ChatMessage, hasStore bool) *toolDemotion {
	p := &toolDemotion{}
	if msg.Role != messages.MessageRoleTool || msg.Content == ToolDeniedContent {
		return p
	}
	if isRecallToolName(msg.ToolName) {
		stub := recallResultStub(msg.ToolName)
		if estimatedStringTokens(stub) >= estimatedStringTokens(msg.Content) {
			return p
		}
		p.content = appendArtifactDescriptors(stub, msg, "", " ")
		p.tokens, p.ok = estimatedStringTokens(p.content), true
		return p
	}
	if !hasStore {
		return p
	}
	ref := textArtifactRef(msg)
	if ref == nil {
		if estimatedStringTokens(msg.Content) <= toolPreviewTokenLimit {
			return p
		}
		prospective := prospectiveTextRef(msg.Content)
		ref = &prospective
		p.inline = true
	}
	p.content = appendArtifactDescriptors(artifactReceipt(*ref), msg, ref.ID, " ")
	p.tokens = estimatedStringTokens(p.content)
	p.ok = p.tokens < estimatedStringTokens(msg.Content)
	return p
}

func (c *projectionCache) demotion(i int, msg messages.ChatMessage, hasStore bool) *toolDemotion {
	if c.demotions == nil {
		c.demotions = make(map[int]*toolDemotion)
	}
	if p, ok := c.demotions[i]; ok {
		return p
	}
	p := planToolDemotion(msg, hasStore)
	c.demotions[i] = p
	return p
}

func (c *projectionCache) retainSelectedImages(history []messages.ChatMessage, selected map[[2]int]bool) {
	if len(c.images) == 0 {
		return
	}
	ids := make(map[string]bool, len(selected))
	for index := range selected {
		if ref := history[index[0]].Parts[index[1]].Artifact; ref != nil {
			ids[ref.ID] = true
		}
	}
	for id, image := range c.images {
		if !ids[id] {
			delete(c.images, id)
			c.imageBytes -= len(image.encoded)
		}
	}
}

func (c *projectionCache) hydrateImage(ctx context.Context, part messages.ContentPart, store artifacts.Store) (messages.ContentPart, error) {
	if part.Type == "image_base64" || part.Type == "image_url" {
		return part, nil
	}
	if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
		return messages.ContentPart{}, fmt.Errorf("invalid image artifact reference")
	}
	ref := part.Artifact
	if store == nil {
		return messages.ContentPart{}, fmt.Errorf("image artifact %s cannot be read without a store", ref.ID)
	}
	cached, ok := c.images[ref.ID]
	if ok && cached.bytes != ref.Bytes {
		return messages.ContentPart{}, fmt.Errorf("read image artifact %s: stored size does not match transcript reference", ref.ID)
	}
	if !ok {
		data, err := readArtifactBytes(ctx, store, ref.ID, ref.Bytes)
		if err != nil {
			return messages.ContentPart{}, fmt.Errorf("read image artifact %s: %w", ref.ID, err)
		}
		cached = cachedProjectionImage{bytes: ref.Bytes, encoded: base64.StdEncoding.EncodeToString(data)}
		// The selection pass releases unselected images first. This fallback
		// also bounds direct uses of hydrateImage outside that pass.
		if c.imageBytes+len(cached.encoded) > maxProjectedEncodedImageBytes {
			c.images, c.imageBytes = nil, 0
		}
		if len(cached.encoded) <= maxProjectedEncodedImageBytes {
			if c.images == nil {
				c.images = make(map[string]cachedProjectionImage)
			}
			c.images[ref.ID] = cached
			c.imageBytes += len(cached.encoded)
		}
	}
	return messages.ContentPart{Type: "image_base64", ImageData: cached.encoded, MimeType: ref.MIMEType, FileName: ref.Name}, nil
}
