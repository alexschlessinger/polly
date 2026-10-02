package llm

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

// imageCache belongs to one Run: the encoded bytes of the stored images its
// requests hydrate, which are immutable, so every request after the first
// that selects an image sends it without reading the store again.
type imageCache struct {
	omit   bool // capability preparation already replaced media with text
	images map[string]cachedProjectionImage
	bytes  int
}

type cachedProjectionImage struct {
	bytes   int64
	encoded string
}

func (c *imageCache) retainSelectedImages(history []messages.ChatMessage, selected map[[2]int]bool) {
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
			c.bytes -= len(image.encoded)
		}
	}
}

func (c *imageCache) hydrateImage(ctx context.Context, part messages.ContentPart, store artifacts.Store) (messages.ContentPart, error) {
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
		if c.bytes+len(cached.encoded) > maxProjectedEncodedImageBytes {
			c.images, c.bytes = nil, 0
		}
		if len(cached.encoded) <= maxProjectedEncodedImageBytes {
			if c.images == nil {
				c.images = make(map[string]cachedProjectionImage)
			}
			c.images[ref.ID] = cached
			c.bytes += len(cached.encoded)
		}
	}
	return messages.ContentPart{Type: "image_base64", ImageData: cached.encoded, MimeType: ref.MIMEType, FileName: ref.Name}, nil
}
