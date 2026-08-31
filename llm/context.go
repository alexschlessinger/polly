package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	toolInlineTokenLimit  = 10_000
	toolPreviewTokenLimit = 500
	maxHydratedArtifact   = 64 << 20

	// estimatedImageTokens is the flat per-image cost estimate. Polly caps
	// uploads at 1568px on the long edge, and provider vision billing on an
	// image that size commonly lands near 2000 tokens.
	estimatedImageTokens = 2000

	// omissionQuantumDivisor sets the omission batch size to a fifth of the
	// budget; see omissionFront.
	omissionQuantumDivisor = 5

	// Aggregate caps on the images one projected request may carry, mirroring
	// the tightest native-client request shape: 100 images per request and a
	// bounded total of base64-encoded image bytes.
	maxProjectedRequestImages     = 100
	maxProjectedEncodedImageBytes = 16 << 20
)

var stableImageTokenPattern = regexp.MustCompile(`\[image (?:#[0-9]+|sha256:[0-9a-f]{64})\]`)

// ProjectionStats describes the final provider-visible view without changing
// the complete durable transcript returned by Agent.Run.
type ProjectionStats struct {
	EstimatedTokens      int
	OmittedExchanges     int
	CompactedToolResults int
	HydratedImages       int
	artifactRefs         []artifacts.Ref
	toolSpills           []toolResultSpill
}

type toolResultSpill struct {
	ToolCallID string
	ToolName   string
	Content    string
	Ref        artifacts.Ref
	// Receipt is the exact provider-visible form the spilling projection sent;
	// applyDurableToolSpills persists it verbatim so the durable final form and
	// the sent form never diverge.
	Receipt string
}

// ContextLimitError means the active exchange could not fit even after all
// deterministic reductions were applied.
type ContextLimitError struct {
	EstimatedTokens int
	Limit           int
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("active exchange needs about %d tokens, exceeding the %d-token context budget", e.EstimatedTokens, e.Limit)
}

func projectMessages(ctx context.Context, history []messages.ChatMessage, maxTokens int, store artifacts.Store, transcriptReadable bool) ([]messages.ChatMessage, ProjectionStats, error) {
	projected := cloneMessages(history)
	projected = filterInternalMessages(projected)
	var stats ProjectionStats

	var err error
	front := toolCompactionFront(projected, maxTokens, store)
	projected, stats.CompactedToolResults, err = projectToolResults(ctx, projected, store, front)
	if err != nil {
		return nil, stats, err
	}
	// Capture refs and media descriptors before omission and image selection
	// can drop their parts: refs keep omitted-exchange artifacts indexed, and
	// the descriptors let a later spill build the same final form that
	// applyDurableToolSpills persists.
	stats.artifactRefs = artifactRefsInMessages(projected)
	spillDescriptors := toolMediaDescriptorsByCall(projected)
	projected, stats.HydratedImages, err = projectImages(ctx, projected, store)
	if err != nil {
		return nil, stats, err
	}

	stats.EstimatedTokens = estimateProjectedTokens(projected)
	if maxTokens <= 0 || stats.EstimatedTokens <= maxTokens {
		return stripArtifactParts(projected), stats, nil
	}

	marker := projectionMarker(store != nil, transcriptReadable)
	if front := omissionFront(projected, marker, maxTokens); front > 0 {
		users := realUserIndexes(projected)
		projected = omitExchangePreservingSystems(projected, users[0], users[front])
		projected = addProjectionMarker(projected, marker)
		stats.OmittedExchanges = front
		stats.EstimatedTokens = estimateProjectedTokens(projected)
	}
	if stats.EstimatedTokens > maxTokens {
		spilled, spills, spillErr := spillActiveToolResults(ctx, projected, maxTokens, store, spillDescriptors)
		if spillErr != nil {
			return nil, stats, spillErr
		}
		stats.toolSpills = spills
		stats.CompactedToolResults += spilled
		stats.EstimatedTokens = estimateProjectedTokens(projected)
	}
	if stats.EstimatedTokens > maxTokens {
		return nil, stats, &ContextLimitError{EstimatedTokens: stats.EstimatedTokens, Limit: maxTokens}
	}
	for _, spill := range stats.toolSpills {
		stats.artifactRefs = appendArtifactRef(stats.artifactRefs, spill.Ref)
	}
	return stripArtifactParts(projected), stats, nil
}

func appendArtifactRef(refs []artifacts.Ref, ref artifacts.Ref) []artifacts.Ref {
	for i := range refs {
		if refs[i].ID == ref.ID {
			if artifactKindPriority(ref.Kind) > artifactKindPriority(refs[i].Kind) {
				refs[i] = ref
			}
			return refs
		}
	}
	return append(refs, ref)
}

func artifactRefsInMessages(history []messages.ChatMessage) []artifacts.Ref {
	seen := make(map[string]bool)
	var refs []artifacts.Ref
	for _, msg := range history {
		for _, part := range msg.Parts {
			if part.Artifact == nil {
				continue
			}
			if seen[part.Artifact.ID] {
				refs = appendArtifactRef(refs, *part.Artifact)
				continue
			}
			seen[part.Artifact.ID] = true
			refs = append(refs, *part.Artifact)
		}
	}
	return refs
}

// omissionFront chooses how many leading completed exchanges to omit once the
// projection exceeds the budget. The front may only land on quantized
// candidates: exchange boundaries where the cumulative omittable size from the
// transcript start has grown by at least a fifth of the budget since the
// previous candidate. Each jump therefore frees at least one quantum, so a
// saturated session re-omits in batches instead of sliding one exchange per
// turn, and the provider-visible prefix stays byte-stable between jumps.
// Candidates depend only on the transcript prefix, which is append-only, so
// the front remains a pure function of (history, budget).
func omissionFront(projected []messages.ChatMessage, marker string, maxTokens int) int {
	users := realUserIndexes(projected)
	if len(users) <= 1 {
		return 0
	}
	withMarker := estimateProjectedTokens(addProjectionMarker(cloneMessages(projected), marker))
	quantum := max(1, maxTokens/omissionQuantumDivisor)
	cum := 0
	lastCandidate := 0
	for j := 0; j+1 < len(users); j++ {
		for _, msg := range projected[users[j]:users[j+1]] {
			if msg.Role != messages.MessageRoleSystem {
				cum += estimateProjectedMessageTokens(msg)
			}
		}
		// The maximal front is always a candidate so omission can reach it.
		if cum-lastCandidate < quantum && j+2 < len(users) {
			continue
		}
		lastCandidate = cum
		if withMarker-cum <= maxTokens {
			return j + 1
		}
	}
	return len(users) - 1
}

// projectionMarker is constant text: a count would rewrite the system message
// on every additional omission and invalidate the provider's cached prefix.
func projectionMarker(artifactsListable, transcriptReadable bool) string {
	marker := "[Context projection: earlier completed exchanges omitted."
	if transcriptReadable {
		marker += " The full conversation, including omitted exchanges, remains readable: call read_transcript to page or search it."
	} else {
		marker += " The user's local transcript retains the omitted content."
	}
	if artifactsListable {
		marker += " Artifacts referenced by omitted content remain readable: call list_artifacts to enumerate them and read_artifact to inspect one."
	}
	return marker + "]"
}

// toolCompactionFront chooses the message-index boundary below which completed
// exchanges' tool results demote to receipts and stubs. Demotion rewrites
// messages in place, so every advance invalidates the provider's cached prefix
// at the rewrite point. The front therefore stays at zero while the projection
// fits its budget — demotion there would spend a full-price prefix rewrite to
// save tokens the cache discount already made cheap — and once over budget it
// may only land on quantized candidates: exchange boundaries where the
// cumulative demotion savings have grown by at least a fifth of the budget
// since the previous candidate. It picks the first candidate that brings the
// projection under budget, or the maximal front (every completed exchange)
// when none does. Candidates depend only on durable message forms, which are
// fixed once an exchange completes, so the front is a pure function of
// (history, budget, store presence) that only advances as the session grows.
// The gate prices the pre-hydration projection; image-heavy saturation is
// still caught by the post-hydration budget checks in projectMessages.
func toolCompactionFront(projected []messages.ChatMessage, maxTokens int, store artifacts.Store) int {
	if maxTokens <= 0 {
		return 0
	}
	users := realUserIndexes(projected)
	if len(users) <= 1 {
		return 0
	}
	hasStore := store != nil
	// Oversized inline results never ship raw: the birth-form zone of
	// projectToolResults externalizes them to bounded previews, so both the
	// gate estimate and the demotion savings price them at the preview bound.
	previewBounded := func(msg messages.ChatMessage) bool {
		return hasStore && msg.Role == messages.MessageRoleTool && msg.Content != ToolDeniedContent &&
			!isRecallToolName(msg.ToolName) && textArtifactRef(msg) == nil &&
			estimatedStringTokens(msg.Content) > toolInlineTokenLimit
	}
	estimate := 0
	for _, msg := range projected {
		tokens := estimateProjectedMessageTokens(msg)
		if previewBounded(msg) {
			tokens -= estimatedStringTokens(msg.Content) - toolPreviewTokenLimit
		}
		estimate += tokens
	}
	if estimate <= maxTokens {
		return 0
	}
	quantum := max(1, maxTokens/omissionQuantumDivisor)
	cum := 0
	lastCandidate := 0
	for j := 0; j+1 < len(users); j++ {
		for _, msg := range projected[users[j]:users[j+1]] {
			form, _, ok := demotedToolResultForm(msg, hasStore)
			if !ok {
				continue
			}
			visible := estimatedStringTokens(msg.Content)
			if previewBounded(msg) {
				visible = toolPreviewTokenLimit
			}
			if saved := visible - estimatedStringTokens(form); saved > 0 {
				cum += saved
			}
		}
		// The maximal front is always a candidate so demotion can reach every
		// completed exchange before omission has to drop whole ones.
		if cum-lastCandidate < quantum && j+2 < len(users) {
			continue
		}
		lastCandidate = cum
		if estimate-cum <= maxTokens {
			return users[j+1]
		}
	}
	return users[len(users)-1]
}

// projectToolResults is the completed-exchange compaction pass. Results at or
// past the front keep their birth forms byte-identical (inline bytes, or an
// artifact preview minted when the tool ran); results before the front demote
// to their most compact recoverable form: recall results collapse to a
// constant stub (their content is reproducible on demand), artifact-backed
// results collapse from preview to receipt, and large inline results are
// idempotently externalized to a content-addressed artifact behind the same
// receipt. The front comes from toolCompactionFront: zero while the projection
// fits its budget, so an unsaturated session ships every exchange in birth
// form and the provider's cached prefix survives turn boundaries. Legacy
// transcripts' oversized inline results in the birth-form zone are
// externalized with a preview built from the in-hand bytes, as birth would
// have done.
func projectToolResults(ctx context.Context, history []messages.ChatMessage, store artifacts.Store, front int) ([]messages.ChatMessage, int, error) {
	compacted := 0
	for i := range history {
		msg := &history[i]
		if msg.Role != messages.MessageRoleTool || msg.Content == ToolDeniedContent {
			continue
		}
		completed := i < front
		if isRecallToolName(msg.ToolName) {
			if completed {
				if form, _, ok := demotedToolResultForm(*msg, store != nil); ok {
					msg.Content = form
					compacted++
				}
			}
			continue
		}
		ref := textArtifactRef(*msg)
		if ref != nil && store == nil {
			return nil, compacted, fmt.Errorf("tool artifact %s cannot be read without a store", ref.ID)
		}
		if completed && store != nil {
			form, blob, ok := demotedToolResultForm(*msg, true)
			if !ok {
				continue
			}
			if blob != nil {
				stored, err := store.Put(ctx, *blob)
				if err != nil {
					return nil, compacted, fmt.Errorf("store projected tool artifact for %q: %w", msg.ToolName, err)
				}
				msg.Parts = append(msg.Parts, messages.ContentPart{Type: "artifact", Artifact: &stored})
			}
			msg.Content = form
			compacted++
			continue
		}
		if ref == nil && store != nil && estimatedStringTokens(msg.Content) > toolInlineTokenLimit {
			stored, err := store.Put(ctx, artifacts.Blob{
				Kind: artifacts.KindText, MIMEType: "text/plain", Name: toolArtifactName(*msg), Data: []byte(msg.Content),
			})
			if err != nil {
				return nil, compacted, fmt.Errorf("store projected tool artifact for %q: %w", msg.ToolName, err)
			}
			head, tail := previewWindows([]byte(msg.Content))
			msg.Parts = append(msg.Parts, messages.ContentPart{Type: "artifact", Artifact: &stored})
			msg.Content = artifactPreviewWithDescriptors(stored, head, tail, *msg)
			compacted++
		}
	}
	return history, compacted, nil
}

// demotedToolResultForm is the single source of truth for demotion: the exact
// provider-visible form a tool result takes once the compaction front passes
// it, plus the blob to store when demotion must first externalize inline
// content. toolCompactionFront prices candidates with it and
// projectToolResults applies it, so the two can never disagree about bytes.
// It reports false when demotion would not shrink the result.
func demotedToolResultForm(msg messages.ChatMessage, hasStore bool) (string, *artifacts.Blob, bool) {
	if msg.Role != messages.MessageRoleTool || msg.Content == ToolDeniedContent {
		return "", nil, false
	}
	if isRecallToolName(msg.ToolName) {
		stub := recallResultStub(msg.ToolName)
		if estimatedStringTokens(stub) >= estimatedStringTokens(msg.Content) {
			return "", nil, false
		}
		return appendArtifactDescriptors(stub, msg, "", " "), nil, true
	}
	if !hasStore {
		return "", nil, false
	}
	if ref := textArtifactRef(msg); ref != nil {
		form := appendArtifactDescriptors(artifactReceipt(*ref), msg, ref.ID, " ")
		if estimatedStringTokens(form) >= estimatedStringTokens(msg.Content) {
			return "", nil, false
		}
		return form, nil, true
	}
	if msg.Content == "" || estimatedStringTokens(msg.Content) <= toolPreviewTokenLimit {
		return "", nil, false
	}
	blob := artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: toolArtifactName(msg), Data: []byte(msg.Content)}
	prospective := artifacts.RefForBlob(blob)
	form := appendArtifactDescriptors(artifactReceipt(prospective), msg, prospective.ID, " ")
	if estimatedStringTokens(form) >= estimatedStringTokens(msg.Content) {
		return "", nil, false
	}
	return form, &blob, true
}

func isRecallToolName(name string) bool {
	return name == "read_artifact" || name == "list_artifacts" || name == "read_transcript"
}

func recallResultStub(toolName string) string {
	switch toolName {
	case "list_artifacts":
		return "[list_artifacts result elided; call list_artifacts again for the current catalog.]"
	case "read_transcript":
		return "[read_transcript result elided to save space; the call above shows its arguments. Call read_transcript again to re-read.]"
	}
	return "[read_artifact result elided to save space; the call above shows its arguments. Call read_artifact again to re-read.]"
}

// previewWindows bounds the head and tail slices fed to artifactPreview, which
// trims them to the token budget on UTF-8-safe boundaries.
func previewWindows(data []byte) ([]byte, []byte) {
	window := min(len(data), toolPreviewTokenLimit*4)
	return data[:window], data[len(data)-window:]
}

// artifactBirthPreview is a stored tool result's permanent provider-visible
// form, computed once from the bytes in hand.
func artifactBirthPreview(ref artifacts.Ref, data []byte) string {
	head, tail := previewWindows(data)
	return artifactPreview(ref, head, tail, toolPreviewTokenLimit*4)
}

// ValidateImageProjection runs the deterministic image-selection phase of the
// provider projection over the given history without reading any artifact
// bytes. It reports the errors a subsequent Agent.Run would hit regardless of
// store state — unresolvable or ambiguous image references and the aggregate
// request caps — so callers can reject a prompt before durably persisting it.
func ValidateImageProjection(history []messages.ChatMessage) error {
	visible := make([]messages.ChatMessage, 0, len(history))
	for _, msg := range history {
		if msg.Role != messages.MessageRoleInternal {
			visible = append(visible, msg)
		}
	}
	_, err := selectProjectedImages(visible)
	return err
}

// EstimateMessageTokens estimates the provider-visible token cost of a single
// message with the same heuristic context projection uses for its budget.
func EstimateMessageTokens(msg messages.ChatMessage) int {
	return estimateProjectedMessageTokens(msg)
}

type imageSelection struct {
	latestUser int
	selected   map[[2]int]bool
}

func selectProjectedImages(history []messages.ChatMessage) (imageSelection, error) {
	latestUser := -1
	for i := len(history) - 1; i >= 0; i-- {
		if isRealUser(history[i]) {
			latestUser = i
			break
		}
	}
	latestText := ""
	if latestUser >= 0 {
		latestText = messageText(history[latestUser])
	}
	lastAssistant := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == messages.MessageRoleAssistant {
			lastAssistant = i
			break
		}
	}

	type candidate struct {
		message    int
		part       int
		name       string
		imageToken string
		reference  string
		id         string
		direct     bool
	}
	var candidates []candidate
	byName := make(map[string][]int)
	byReference := make(map[string][]int)
	byID := make(map[string][]int)
	directNames := make(map[string]bool)
	for i, msg := range history {
		for j, part := range msg.Parts {
			if !isImagePart(part) {
				continue
			}
			c := candidate{
				message:    i,
				part:       j,
				name:       part.FileName,
				imageToken: part.Reference,
				direct: i == latestUser ||
					(history[i].Role == messages.MessageRoleTool && i > latestUser && i > lastAssistant),
			}
			if part.Artifact != nil {
				c.name = part.Artifact.Name
				c.imageToken = part.Artifact.ImageToken
				c.reference = part.Artifact.Reference
				c.id = part.Artifact.ID
			}
			idx := len(candidates)
			candidates = append(candidates, c)
			if c.name != "" {
				byName[c.name] = append(byName[c.name], idx)
				if c.direct {
					directNames[c.name] = true
				}
			}
			aliases := []string{c.imageToken, c.reference, part.Reference}
			seenAliases := make(map[string]bool, len(aliases))
			for _, alias := range aliases {
				if alias == "" || seenAliases[alias] {
					continue
				}
				seenAliases[alias] = true
				byReference[alias] = append(byReference[alias], idx)
			}
			if c.id != "" {
				byID[c.id] = append(byID[c.id], idx)
			}
		}
	}

	selectedCandidates := make(map[int]bool)
	for i, c := range candidates {
		if c.direct {
			selectedCandidates[i] = true
		}
	}
	identity := func(index int) string {
		c := candidates[index]
		if c.id != "" {
			return "id:" + c.id
		}
		return fmt.Sprintf("part:%d:%d", c.message, c.part)
	}
	selectUnique := func(indexes []int, label, ambiguityHint string) error {
		unique := make(map[string]int)
		for _, index := range indexes {
			// Keep the newest occurrence of identical immutable bytes.
			unique[identity(index)] = index
		}
		if len(unique) > 1 {
			return fmt.Errorf("%s %q matches multiple stored images; %s", label, ambiguityHint, "use its stable image token or artifact ID")
		}
		for _, index := range unique {
			selectedCandidates[index] = true
		}
		return nil
	}

	seenStableTokens := make(map[string]bool)
	for _, token := range stableImageTokenPattern.FindAllString(latestText, -1) {
		if seenStableTokens[token] {
			continue
		}
		seenStableTokens[token] = true
		indexes := byReference[token]
		if len(indexes) == 0 {
			return imageSelection{}, fmt.Errorf("image reference %q is not available in this session", token)
		}
		if err := selectUnique(indexes, "image reference", token); err != nil {
			return imageSelection{}, err
		}
	}
	references := make([]string, 0, len(byReference))
	for reference := range byReference {
		references = append(references, reference)
	}
	sort.Strings(references)
	for _, reference := range references {
		indexes := byReference[reference]
		if seenStableTokens[reference] || !strings.Contains(latestText, reference) {
			continue
		}
		if err := selectUnique(indexes, "image reference", reference); err != nil {
			return imageSelection{}, err
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		indexes := byID[id]
		if !strings.Contains(latestText, id) {
			continue
		}
		if err := selectUnique(indexes, "image artifact ID", id); err != nil {
			return imageSelection{}, err
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		indexes := byName[name]
		if directNames[name] || !strings.Contains(latestText, name) {
			continue
		}
		if err := selectUnique(indexes, "image filename", name); err != nil {
			return imageSelection{}, err
		}
	}

	// The same immutable image can appear more than once (for example, when
	// read_artifact returns an image the current prompt already referenced).
	// Hydrate it only once, preferring the current user message, then a newly
	// returned tool image, then the newest historical occurrence.
	winners := make(map[string]int, len(selectedCandidates))
	for index := range candidates {
		if !selectedCandidates[index] {
			continue
		}
		key := identity(index)
		current, ok := winners[key]
		if !ok {
			winners[key] = index
			continue
		}
		candidatePriority := imageCandidatePriority(candidates[index].message, candidates[index].direct, latestUser)
		currentPriority := imageCandidatePriority(candidates[current].message, candidates[current].direct, latestUser)
		if candidatePriority > currentPriority ||
			(candidatePriority == currentPriority && candidates[index].message > candidates[current].message) {
			winners[key] = index
		}
	}
	selected := make(map[[2]int]bool, len(winners))
	totalImages := 0
	totalEncoded := 0
	for _, index := range winners {
		c := candidates[index]
		selected[[2]int{c.message, c.part}] = true
		totalImages++
		part := history[c.message].Parts[c.part]
		switch {
		case part.Artifact != nil:
			totalEncoded += base64.StdEncoding.EncodedLen(int(max(part.Artifact.Bytes, 0)))
		case part.ImageData != "":
			totalEncoded += len(part.ImageData)
		case strings.HasPrefix(part.ImageURL, "data:"):
			if comma := strings.IndexByte(part.ImageURL, ','); comma >= 0 {
				totalEncoded += len(part.ImageURL) - comma - 1
			}
		}
	}
	if totalImages > maxProjectedRequestImages {
		return imageSelection{}, fmt.Errorf("prompt selects %d images for the request; portable maximum is %d", totalImages, maxProjectedRequestImages)
	}
	if totalEncoded > maxProjectedEncodedImageBytes {
		return imageSelection{}, fmt.Errorf("selected images would send about %d encoded bytes; portable limit is %d MiB", totalEncoded, maxProjectedEncodedImageBytes>>20)
	}
	return imageSelection{latestUser: latestUser, selected: selected}, nil
}

func projectImages(ctx context.Context, history []messages.ChatMessage, store artifacts.Store) ([]messages.ChatMessage, int, error) {
	selection, err := selectProjectedImages(history)
	if err != nil {
		return nil, 0, err
	}
	latestUser := selection.latestUser
	selected := selection.selected

	var referenced []messages.ContentPart
	var currentToolImages []messages.ContentPart
	hydrated := 0
	for i := range history {
		parts := make([]messages.ContentPart, 0, len(history[i].Parts))
		selectedCurrentImage := false
		for j, part := range history[i].Parts {
			if !isImagePart(part) {
				parts = append(parts, part)
				continue
			}
			if !selected[[2]int{i, j}] {
				continue
			}
			hydratedPart, err := hydrateImagePart(ctx, part, store)
			if err != nil {
				return nil, hydrated, err
			}
			hydrated++
			switch {
			case i == latestUser:
				parts = append(parts, hydratedPart)
				selectedCurrentImage = true
			case history[i].Role == messages.MessageRoleTool && i > latestUser:
				currentToolImages = append(currentToolImages, hydratedPart)
			default:
				referenced = append(referenced, hydratedPart)
			}
		}
		history[i].Parts = parts
		if i == latestUser && selectedCurrentImage {
			promoteMessageContentToTextPart(&history[i])
		}
	}

	if latestUser >= 0 && len(referenced) > 0 {
		promoteMessageContentToTextPart(&history[latestUser])
		history[latestUser].Parts = append(history[latestUser].Parts, referenced...)
	}
	if len(currentToolImages) > 0 {
		parts := []messages.ContentPart{{Type: "text", Text: "Images returned by the preceding tool call(s):"}}
		parts = append(parts, currentToolImages...)
		history = append(history, messages.ChatMessage{
			Role:     messages.MessageRoleUser,
			Parts:    parts,
			Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true},
		})
	}
	return history, hydrated, nil
}

func messageText(msg messages.ChatMessage) string {
	var b strings.Builder
	if msg.Content != "" {
		b.WriteString(msg.Content)
	}
	for _, part := range msg.Parts {
		if part.Type != "text" || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

func imageCandidatePriority(message int, direct bool, latestUser int) int {
	if message == latestUser {
		return 3
	}
	if direct {
		return 2
	}
	return 1
}

func promoteMessageContentToTextPart(msg *messages.ChatMessage) {
	if msg == nil || msg.Content == "" {
		return
	}
	msg.Parts = append([]messages.ContentPart{{Type: "text", Text: msg.Content}}, msg.Parts...)
	msg.Content = ""
}

func hydrateImagePart(ctx context.Context, part messages.ContentPart, store artifacts.Store) (messages.ContentPart, error) {
	if part.Type == "image_base64" || part.Type == "image_url" {
		return part, nil
	}
	if part.Artifact == nil || part.Artifact.Kind != artifacts.KindImage {
		return messages.ContentPart{}, fmt.Errorf("invalid image artifact reference")
	}
	if store == nil {
		return messages.ContentPart{}, fmt.Errorf("image artifact %s cannot be read without a store", part.Artifact.ID)
	}
	data, err := readArtifactBytes(ctx, store, part.Artifact.ID, part.Artifact.Bytes)
	if err != nil {
		return messages.ContentPart{}, fmt.Errorf("read image artifact %s: %w", part.Artifact.ID, err)
	}
	return messages.ContentPart{
		Type: "image_base64", ImageData: base64.StdEncoding.EncodeToString(data), MimeType: part.Artifact.MIMEType, FileName: part.Artifact.Name,
	}, nil
}

func readArtifactBytes(ctx context.Context, store artifacts.Store, id string, expected int64) ([]byte, error) {
	if expected < 0 {
		return nil, fmt.Errorf("invalid artifact size")
	}
	if expected > maxHydratedArtifact {
		return nil, fmt.Errorf("artifact is too large to hydrate (%d bytes)", expected)
	}
	r, err := store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(r, expected+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if int64(len(data)) != expected {
		return nil, fmt.Errorf("stored size does not match transcript reference")
	}
	return data, nil
}

func textArtifactRef(msg messages.ChatMessage) *artifacts.Ref {
	for _, part := range msg.Parts {
		if part.Artifact != nil && part.Artifact.Kind == artifacts.KindText {
			ref := *part.Artifact
			return &ref
		}
	}
	return nil
}

func artifactReceipt(ref artifacts.Ref) string {
	return fmt.Sprintf("[tool output stored as artifact %s; %d bytes; %d lines. Use read_artifact to inspect it.]", ref.ID, ref.Bytes, ref.Lines)
}

func artifactMediaDescriptor(ref artifacts.Ref) string {
	switch ref.Kind {
	case artifacts.KindImage:
		if token := sanitizeArtifactDescriptor(ref.ImageToken); token != "" {
			return fmt.Sprintf("[image artifact %s; reference %s; %d bytes]", ref.ID, token, ref.Bytes)
		}
		return fmt.Sprintf("[image artifact %s; %d bytes]", ref.ID, ref.Bytes)
	case artifacts.KindBinary:
		return fmt.Sprintf("[binary artifact %s; %s; %d bytes; payload not inserted]", ref.ID, sanitizeArtifactDescriptor(ref.MIMEType), ref.Bytes)
	default:
		return ""
	}
}

func sanitizeArtifactDescriptor(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 256 {
		value = value[:safeUTF8Boundary(value, 256)]
	}
	return value
}

func appendArtifactDescriptors(content string, msg messages.ChatMessage, excludedID, separator string) string {
	descriptors := artifactDescriptors(msg, excludedID)
	if len(descriptors) == 0 {
		return content
	}
	return strings.TrimSpace(content + separator + strings.Join(descriptors, separator))
}

func artifactDescriptors(msg messages.ChatMessage, excludedID string) []string {
	var descriptors []string
	seen := make(map[string]bool)
	for _, part := range msg.Parts {
		if part.Artifact == nil || (part.Artifact.Kind == artifacts.KindText && part.Artifact.ID == excludedID) {
			continue
		}
		key := string(part.Artifact.Kind) + "\x00" + part.Artifact.ID + "\x00" + part.Artifact.ImageToken
		if seen[key] {
			continue
		}
		seen[key] = true
		if descriptor := artifactMediaDescriptor(*part.Artifact); descriptor != "" {
			descriptors = append(descriptors, descriptor)
		}
	}
	return descriptors
}

func artifactPreviewWithDescriptors(ref artifacts.Ref, headData, tailData []byte, msg messages.ChatMessage) string {
	const maxBytes = toolPreviewTokenLimit * 4
	descriptorText := boundedArtifactDescriptors(artifactDescriptors(msg, ref.ID), maxBytes/3)
	previewBudget := maxBytes
	if descriptorText != "" {
		previewBudget -= len(descriptorText) + 1
	}
	preview := artifactPreview(ref, headData, tailData, previewBudget)
	if descriptorText == "" {
		return preview
	}
	return preview + "\n" + descriptorText
}

func boundedArtifactDescriptors(descriptors []string, limit int) string {
	const marker = "[additional media descriptors omitted]"
	var out strings.Builder
	for i, descriptor := range descriptors {
		separator := 0
		if out.Len() > 0 {
			separator = 1
		}
		if out.Len()+separator+len(descriptor) > limit {
			if out.Len() > 0 && out.Len()+1+len(marker) <= limit {
				out.WriteByte('\n')
			}
			if out.Len()+len(marker) <= limit {
				out.WriteString(marker)
			}
			break
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(descriptor)
	}
	return out.String()
}

func artifactPreview(ref artifacts.Ref, headData, tailData []byte, maxBytes int) string {
	// The first line must be self-framing: a model that has never seen the
	// receipt convention should still read this as its tool's real output,
	// stored whole, deliberately previewed — not as truncated or corrupt data.
	header := fmt.Sprintf("[tool output stored as artifact %s; %d bytes; %d lines. Head/tail preview follows.]\n", ref.ID, ref.Bytes, ref.Lines)
	gap := "\n\n[... middle omitted; use read_artifact (offset/limit/query) to read the rest ...]\n\n"
	available := maxBytes - len(header) - len(gap)
	if available <= 0 {
		return header[:min(len(header), maxBytes)]
	}
	headerBudget := available / 2
	tailBudget := available - headerBudget
	head := strings.ToValidUTF8(string(headData), "�")
	tail := strings.ToValidUTF8(string(tailData), "�")
	if len(head) > headerBudget {
		head = head[:safeUTF8Boundary(head, headerBudget)]
	}
	if len(tail) > tailBudget {
		start := safeUTF8TailBoundary(tail, len(tail)-tailBudget)
		tail = tail[start:]
	}
	return header + head + gap + tail
}

func safeUTF8Boundary(s string, end int) int {
	if end >= len(s) {
		return len(s)
	}
	for end > 0 && (s[end]&0xc0) == 0x80 {
		end--
	}
	return end
}

func safeUTF8TailBoundary(s string, start int) int {
	if start <= 0 {
		return 0
	}
	for start < len(s) && (s[start]&0xc0) == 0x80 {
		start++
	}
	return start
}

func toolArtifactName(msg messages.ChatMessage) string {
	if msg.ToolName == "" {
		return "tool-output.txt"
	}
	return msg.ToolName + ".txt"
}

func isImagePart(part messages.ContentPart) bool {
	return part.Type == "image_base64" || part.Type == "image_url" || part.Type == "image_artifact" ||
		(part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage)
}

func isRealUser(msg messages.ChatMessage) bool {
	if msg.Role != messages.MessageRoleUser {
		return false
	}
	synthetic, _ := msg.Metadata[messages.MetadataKeyAgentSynthetic].(bool)
	return !synthetic
}

func realUserIndexes(history []messages.ChatMessage) []int {
	var indexes []int
	for i, msg := range history {
		if isRealUser(msg) {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func addProjectionMarker(history []messages.ChatMessage, marker string) []messages.ChatMessage {
	for i := range history {
		if history[i].Role == messages.MessageRoleSystem {
			if history[i].Content != "" {
				history[i].Content += "\n\n" + marker
			} else {
				history[i].Content = marker
			}
			return history
		}
	}
	return append([]messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: marker}}, history...)
}

func omitExchangePreservingSystems(history []messages.ChatMessage, start, end int) []messages.ChatMessage {
	out := make([]messages.ChatMessage, 0, len(history)-(end-start))
	out = append(out, history[:start]...)
	for _, msg := range history[start:end] {
		if msg.Role == messages.MessageRoleSystem {
			out = append(out, msg)
		}
	}
	out = append(out, history[end:]...)
	return out
}

// toolMediaDescriptorsByCall snapshots each tool result's media descriptors
// keyed by tool-call ID, before projectImages moves or drops image parts.
func toolMediaDescriptorsByCall(history []messages.ChatMessage) map[string][]string {
	var out map[string][]string
	for _, msg := range history {
		if msg.Role != messages.MessageRoleTool || msg.ToolCallID == "" {
			continue
		}
		if descriptors := artifactDescriptors(msg, ""); len(descriptors) > 0 {
			if out == nil {
				out = make(map[string][]string)
			}
			out[msg.ToolCallID] = descriptors
		}
	}
	return out
}

// withDescriptorList mirrors appendArtifactDescriptors' byte layout for a
// descriptor list captured earlier.
func withDescriptorList(content string, descriptors []string) string {
	if len(descriptors) == 0 {
		return content
	}
	return strings.TrimSpace(content + " " + strings.Join(descriptors, " "))
}

func spillActiveToolResults(ctx context.Context, history []messages.ChatMessage, maxTokens int, store artifacts.Store, descriptors map[string][]string) (int, []toolResultSpill, error) {
	users := realUserIndexes(history)
	if len(users) == 0 {
		return 0, nil, nil
	}
	start := users[len(users)-1]
	lastAssistant := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == messages.MessageRoleAssistant {
			lastAssistant = i
			break
		}
	}
	newlyCompacted := 0
	var spills []toolResultSpill
	for i := start; i < len(history) && estimateProjectedTokens(history) > maxTokens; i++ {
		if history[i].Role != messages.MessageRoleTool || history[i].Content == ToolDeniedContent {
			continue
		}
		if isRecallToolName(history[i].ToolName) {
			// Stubbing a recall the model has not yet acted on would instruct it
			// to re-read under a budget that can never fit the result; leave the
			// newest unseen recall verbatim and fail over to ContextLimitError.
			if i > lastAssistant {
				continue
			}
			stub := withDescriptorList(recallResultStub(history[i].ToolName), descriptors[history[i].ToolCallID])
			if estimatedStringTokens(stub) < estimatedStringTokens(history[i].Content) {
				history[i].Content = stub
				newlyCompacted++
			}
			continue
		}
		ref := textArtifactRef(history[i])
		originalContent := history[i].Content
		mint := false
		var blob artifacts.Blob
		if ref == nil {
			if originalContent == "" || store == nil {
				continue
			}
			blob = artifacts.Blob{
				Kind: artifacts.KindText, MIMEType: "text/plain", Name: toolArtifactName(history[i]), Data: []byte(originalContent),
			}
			prospective := artifacts.RefForBlob(blob)
			ref = &prospective
			mint = true
		}
		form := withDescriptorList(artifactReceipt(*ref), descriptors[history[i].ToolCallID])
		// A receipt no smaller than the content it replaces cannot relieve
		// pressure; leave the result inline rather than mint an artifact and
		// durably degrade a small, legible result into a receipt.
		if estimatedStringTokens(form) >= estimatedStringTokens(originalContent) {
			continue
		}
		if mint {
			stored, err := store.Put(ctx, blob)
			if err != nil {
				return newlyCompacted, spills, fmt.Errorf("store active tool artifact for %q: %w", history[i].ToolName, err)
			}
			history[i].Parts = append(history[i].Parts, messages.ContentPart{Type: "artifact", Artifact: &stored})
			spills = append(spills, toolResultSpill{
				ToolCallID: history[i].ToolCallID, ToolName: history[i].ToolName, Content: originalContent, Ref: stored, Receipt: form,
			})
		}
		history[i].Content = form
		newlyCompacted++
	}
	return newlyCompacted, spills, nil
}

func stripArtifactParts(history []messages.ChatMessage) []messages.ChatMessage {
	for i := range history {
		parts := history[i].Parts[:0]
		for _, part := range history[i].Parts {
			if part.Artifact == nil && part.Type != "artifact" && part.Type != "image_artifact" && part.Type != "file" {
				parts = append(parts, part)
			}
		}
		history[i].Parts = parts
	}
	return history
}

func filterInternalMessages(history []messages.ChatMessage) []messages.ChatMessage {
	out := history[:0]
	for _, msg := range history {
		if msg.Role != messages.MessageRoleInternal {
			out = append(out, msg)
		}
	}
	return out
}

func cloneMessages(history []messages.ChatMessage) []messages.ChatMessage {
	out := make([]messages.ChatMessage, len(history))
	for i, msg := range history {
		out[i] = msg
		out[i].Parts = append([]messages.ContentPart(nil), msg.Parts...)
		for j := range out[i].Parts {
			if msg.Parts[j].Artifact != nil {
				ref := *msg.Parts[j].Artifact
				out[i].Parts[j].Artifact = &ref
			}
		}
		out[i].ToolCalls = append([]messages.ChatMessageToolCall(nil), msg.ToolCalls...)
		if msg.Metadata != nil {
			out[i].Metadata = make(map[string]any, len(msg.Metadata))
			for key, value := range msg.Metadata {
				out[i].Metadata[key] = value
			}
		}
	}
	return out
}

func estimateProjectedTokens(history []messages.ChatMessage) int {
	total := 0
	for _, msg := range history {
		total += estimateProjectedMessageTokens(msg)
	}
	return total
}

func estimateProjectedMessageTokens(msg messages.ChatMessage) int {
	total := 4 + estimatedStringTokens(msg.Content) + estimatedStringTokens(msg.Reasoning) + estimatedStringTokens(msg.ToolCallID)
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			total += estimatedStringTokens(part.Text)
		case "image_base64", "image_url":
			total += estimatedImageTokens
		}
	}
	for _, call := range msg.ToolCalls {
		total += estimatedStringTokens(call.Name) + estimatedJSONTokens(call.Arguments)
	}
	return total
}

// estimateToolSchemaTokens estimates the request overhead of tool schemas,
// which share the model's context with the projected messages but are not part
// of them. Agent.Run subtracts this from the projection budget.
func estimateToolSchemaTokens(list []tools.Tool) int {
	total := 0
	for _, tool := range list {
		schema := tool.GetSchema()
		if schema == nil {
			continue
		}
		total += 8
		if raw, err := json.Marshal(schema.Raw); err == nil {
			total += estimatedJSONTokens(string(raw))
		}
	}
	return total
}

func estimatedStringTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// estimatedJSONTokens rates dense JSON at 3 bytes per token; the prose
// heuristic's 4 bytes per token systematically undercounts it.
func estimatedJSONTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 2) / 3
}
