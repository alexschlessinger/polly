package llm

import "github.com/alexschlessinger/pollytool/messages"

// compactionView accounts for the same prepared, image-projected input as a
// provider request, without fetching capabilities or reading image bytes. Text
// stays attributable to its message. Images (and unsupported-image labels) are
// tracked separately because projection can move them beside a covering summary.
type compactionView struct {
	messages   []messages.ChatMessage
	textTokens []int
	images     map[compactionImage]int
	total      int
}

// Image identities are local to accounting, never persisted. An artifact's ID
// survives hydration; inline images and omission labels compare their payloads.
type compactionImage struct {
	id, url, data, label string
}

func projectedCompactionView(req *CompletionRequest, history []messages.ChatMessage) (compactionView, error) {
	view := messages.ModelVisible(history)
	out := compactionView{images: make(map[compactionImage]int)}
	imageTokens := make([]int, len(view))
	charge := func(i int, key compactionImage, tokens int) {
		imageTokens[i] += tokens
		out.images[key] += tokens
	}
	noImages := req.Capabilities != nil && omitsImages(*req.Capabilities)
	if noImages {
		for i, msg := range view {
			for _, part := range msg.Parts {
				if isImagePart(part) {
					label := omittedImageText(part)
					charge(i, compactionImage{label: label}, estimatedStringTokens(label))
				}
			}
		}
	}
	if req.Capabilities != nil {
		copy := *req
		copy.Messages = view
		prepared, _, err := PrepareCapabilities(&copy, *req.Capabilities, false)
		if err != nil {
			return compactionView{}, err
		}
		view = prepared.Messages
	}
	if !noImages {
		selection, err := selectProjectedImages(view)
		if err != nil {
			return compactionView{}, err
		}
		view, _, err = projectSelectedImages(view, selection, func(part messages.ContentPart) (messages.ContentPart, error) {
			// Image estimation is flat: artifact bytes are unnecessary. Keep
			// the reference so its identity remains comparable after relocation.
			if part.Artifact != nil {
				part.Type = "image_base64"
			}
			return part, nil
		})
		if err != nil {
			return compactionView{}, err
		}
		imageTokens = make([]int, len(view))
		for i, msg := range view {
			for _, part := range msg.Parts {
				if !isImagePart(part) {
					continue
				}
				key := compactionImage{url: part.ImageURL, data: part.ImageData}
				if part.Artifact != nil {
					key = compactionImage{id: part.Artifact.ID}
				}
				charge(i, key, messages.EstimatedImageTokens)
			}
		}
	}
	out.messages, out.textTokens = view, make([]int, len(view))
	estimate := messageEstimator(req)
	for i, msg := range view {
		tokens := estimate(msg)
		out.total += tokens
		out.textTokens[i] = tokens - imageTokens[i]
	}
	return out, nil
}
