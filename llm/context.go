package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

const (
	toolInlineTokenLimit  = 10_000
	toolPreviewTokenLimit = 500
	maxHydratedArtifact   = 64 << 20

	// Aggregate caps on the images one projected request may carry: the
	// portable request shape messages enforces on model-visible history.
	maxProjectedRequestImages     = messages.MaxPortableRequestImages
	maxProjectedEncodedImageBytes = messages.MaxEncodedImageHistoryBytes
)

var stableImageTokenPattern = regexp.MustCompile(`\[image (?:#[0-9]+|sha256:[0-9a-f]{64})\]`)

// ProjectionStats describes a request as it was sent, without changing the
// complete durable transcript returned by Agent.Run.
type ProjectionStats struct {
	EstimatedTokens int
	// RequestEstimatedTokens includes tool-schema overhead and therefore
	// estimates the complete provider input, not just projected messages.
	RequestEstimatedTokens int
	// CountedTokens is the request's size in the provider's count where one
	// covers it: the input the last response reported, moved by how far this
	// request's estimate is from the estimate of the request that response
	// answered. It is RequestEstimatedTokens when no reported count covers
	// the request. Compaction is triggered by it. The agent sets it, and
	// Counted, which reports whether a provider's count covers it.
	CountedTokens  int
	Counted        bool
	HydratedImages int
	// Budget and MaxTokens are the input budget the request was sized
	// against, 0 for unlimited, and the output limit it asked for, 0 for the
	// provider's default, as the agent applied them: clamped to the model's
	// window (Window, 0 when unknown) and to what a provider's rejection
	// showed, which Learned reports.
	Budget, MaxTokens int
	Window            int
	Learned           bool
}

// ContextLimitError means a request does not fit its context budget even
// after compaction: its system prompt, tool schemas and the turn compaction
// keeps are too large.
type ContextLimitError struct {
	EstimatedTokens int
	Limit           int
}

func (e *ContextLimitError) Error() string {
	return fmt.Sprintf("request needs about %d tokens, exceeding the %d-token context budget", e.EstimatedTokens, e.Limit)
}

// projectionTools is what requests need to know about the agent's tools:
// whether the transcript can be read back, and which tools' results are
// recall results whose cleared form is a stub telling the model to re-call.
type projectionTools struct {
	transcriptReadable bool
	recall             recallStubs
	// notes keeps cleared notes as they are made, for a run's later
	// requests; nil keeps none (see clearedNote).
	notes map[clearedKey]string
}

// clearedKey is what a cleared note, less its media descriptors, depends
// on: the result's content, and whether the note may recommend
// read_transcript.
type clearedKey struct {
	content  string
	readable bool
}

func projectionToolsFor(list []tools.Tool) projectionTools {
	return projectionTools{
		recall: recallStubsFor(list),
		transcriptReadable: slices.ContainsFunc(list, func(tool tools.Tool) bool {
			return tool.GetName() == BuiltinReadTranscript
		}),
	}
}

// recallStubs maps a recall tool's name to the stub its cleared result
// becomes. A nil map marks no tool as recall.
type recallStubs map[string]string

func (r recallStubs) stub(name string) (string, bool) {
	stub, ok := r[name]
	return stub, ok
}

// recallStubsFor collects the recall stubs of the tools that declare one.
func recallStubsFor(list []tools.Tool) recallStubs {
	var stubs recallStubs
	for _, tool := range list {
		if stub, ok := tools.RecallStub(tool); ok {
			if stubs == nil {
				stubs = make(recallStubs)
			}
			stubs[tool.GetName()] = stub
		}
	}
	return stubs
}

// reasoningReplayKeys are the metadata keys under which providers keep
// reasoning they replay: signed, encrypted or signature-bound state that is
// sent back whole with the messages it belongs to.
var reasoningReplayKeys = []string{
	anthropic.ThinkingBlocksKey,
	openai.ResponsesReasoningItemsKey,
	openai.ResponsesReasoningModelKey,
	gemini.ThoughtSignaturesKey,
	openrouter.MetadataKey,
}

// projectCompletionRequest resolves req's history into what one provider
// call carries: skills resolved, internal messages dropped, the images the
// latest exchange selects hydrated from the store, and artifact references
// stripped. It shrinks nothing; compaction keeps requests within budget. A
// nil state projects once without cross-request reuse.
func projectCompletionRequest(ctx context.Context, req *CompletionRequest, store artifacts.Store, state *runState) ([]messages.ChatMessage, ProjectionStats, error) {
	images := &imageCache{}
	var shape *requestShapeCache
	if state != nil {
		images, shape = state.images, state.shape
	}
	history := req.Messages
	if req.Skills != nil && !req.Skills.IsEmpty() {
		history = req.ResolvedMessages()
	}
	// Own the message slice; transformations copy nested slices only when
	// changing them. The durable strings and untouched containers are shared.
	// Image selection reports what ValidateImageProjection would.
	projected, hydrated, err := projectImages(ctx, messages.ModelVisible(history), store, images)
	if err != nil {
		return nil, ProjectionStats{}, err
	}
	stats := ProjectionStats{HydratedImages: hydrated}
	estimate := messageEstimator(req)
	for _, msg := range projected {
		stats.EstimatedTokens += estimate(msg)
	}
	stats.RequestEstimatedTokens = stats.EstimatedTokens + estimateRequestToolSchemaTokens(req, shape)
	return stripArtifactParts(projected), stats, nil
}

// previewWindows bounds the head and tail slices fed to artifactPreview, which
// trims them to the token budget on UTF-8-safe boundaries.
func previewWindows(data []byte) ([]byte, []byte) {
	window := min(len(data), toolPreviewTokenLimit*4)
	return data[:window], data[len(data)-window:]
}

// ValidateImageProjection runs the deterministic image-selection phase of the
// provider projection over the given history without reading any artifact
// bytes. It reports the errors a subsequent Agent.Run would hit regardless of
// store state — unresolvable or ambiguous image references and the aggregate
// request caps — so callers can reject a prompt before durably persisting it.
func ValidateImageProjection(history []messages.ChatMessage) error {
	_, err := selectProjectedImages(messages.ModelVisible(history))
	return err
}

// EstimateMessageTokens estimates the provider-visible token cost of a single
// message with the same heuristic compaction uses where no provider count
// covers a request, charging plain reasoning as a provider that replays it
// would.
func EstimateMessageTokens(msg messages.ChatMessage) int {
	return estimateMessageTokensWith(msg, true)
}

type imageSelection struct {
	latestUser int
	selected   map[[2]int]bool
}

// lastIndex returns the index of the last message satisfying pred, or -1.
func lastIndex(history []messages.ChatMessage, pred func(messages.ChatMessage) bool) int {
	for i := len(history) - 1; i >= 0; i-- {
		if pred(history[i]) {
			return i
		}
	}
	return -1
}

func isAssistant(msg messages.ChatMessage) bool { return msg.Role == messages.MessageRoleAssistant }

func selectProjectedImages(history []messages.ChatMessage) (imageSelection, error) {
	latestUser := lastIndex(history, isRealUser)
	latestText := ""
	if latestUser >= 0 {
		latestText = messageText(history[latestUser])
	}
	lastAssistant := lastIndex(history, isAssistant)

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
	for _, reference := range slices.Sorted(maps.Keys(byReference)) {
		if seenStableTokens[reference] || !strings.Contains(latestText, reference) {
			continue
		}
		if err := selectUnique(byReference[reference], "image reference", reference); err != nil {
			return imageSelection{}, err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		if !strings.Contains(latestText, id) {
			continue
		}
		if err := selectUnique(byID[id], "image artifact ID", id); err != nil {
			return imageSelection{}, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if directNames[name] || !strings.Contains(latestText, name) {
			continue
		}
		if err := selectUnique(byName[name], "image filename", name); err != nil {
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

func projectImages(ctx context.Context, history []messages.ChatMessage, store artifacts.Store, cache *imageCache) ([]messages.ChatMessage, int, error) {
	if cache.omit {
		return history, 0, nil
	}
	selection, err := selectProjectedImages(history)
	if err != nil {
		return nil, 0, err
	}
	latestUser := selection.latestUser
	selected := selection.selected
	cache.retainSelectedImages(history, selected)

	var referenced []messages.ContentPart
	var currentToolImages []messages.ContentPart
	hydrated := 0
	for i := range history {
		if !slices.ContainsFunc(history[i].Parts, isImagePart) {
			continue
		}
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
			hydratedPart, err := cache.hydrateImage(ctx, part, store)
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
		parts := history[latestUser].Parts
		history[latestUser].Parts = append(parts[:len(parts):len(parts)], referenced...)
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
	data, readErr := readArtifactData(r, expected)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	return data, nil
}

func readArtifactData(r io.Reader, expected int64) ([]byte, error) {
	if expected < 0 {
		return nil, fmt.Errorf("invalid artifact size")
	}
	if expected > maxHydratedArtifact {
		return nil, fmt.Errorf("artifact is too large to hydrate (%d bytes)", expected)
	}
	data, err := io.ReadAll(io.LimitReader(r, expected+1))
	if err != nil {
		return nil, err
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

// artifactReceipt stands for tool output stored as an artifact: its id and
// counts, and where to read it.
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

// appendArtifactDescriptors appends msg's media descriptors to content,
// space-separated, leaving out the text artifact excludedID.
func appendArtifactDescriptors(content string, msg messages.ChatMessage, excludedID string) string {
	descriptors := artifactDescriptors(msg, excludedID)
	if len(descriptors) == 0 {
		return content
	}
	return strings.TrimSpace(content + " " + strings.Join(descriptors, " "))
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
	// stored whole, deliberately previewed, not as truncated or corrupt data.
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

func isArtifactPart(part messages.ContentPart) bool {
	return part.Artifact != nil || part.Type == "artifact" || part.Type == "image_artifact" || part.Type == "file"
}

// stripArtifactParts drops artifact-backed parts, giving only the messages it
// changes their own part slice.
func stripArtifactParts(history []messages.ChatMessage) []messages.ChatMessage {
	for i := range history {
		if slices.ContainsFunc(history[i].Parts, isArtifactPart) {
			history[i].Parts = slices.DeleteFunc(slices.Clone(history[i].Parts), isArtifactPart)
		}
	}
	return history
}

func cloneMessages(history []messages.ChatMessage) []messages.ChatMessage {
	out := make([]messages.ChatMessage, len(history))
	for i, msg := range history {
		out[i] = msg.Clone()
	}
	return out
}

// messageEstimator estimates what a message costs a request like req,
// charging reasoning as req's provider replays it.
func messageEstimator(req *CompletionRequest) func(messages.ChatMessage) int {
	if !req.IsOpenRouter() {
		replays := providerFor(targetForRequest(req).Provider).replaysReasoning
		return func(msg messages.ChatMessage) int { return estimateMessageTokensWith(msg, replays) }
	}
	endpoint, model := openrouter.Endpoint(req.BaseURL), targetForRequest(req).Model
	return func(msg messages.ChatMessage) int {
		// OpenRouter replays exactly what openrouter.Replay returns, in place
		// of the reasoning the message holds.
		plain, details := openrouter.Replay(msg, endpoint, model)
		n := messages.EstimateMessageTokens(msg) - estimatedStringTokens(msg.Reasoning)
		if details != nil {
			return n + estimatedStringTokens(string(details))
		}
		return n + estimatedStringTokens(plain)
	}
}

// estimateMessageTokensWith estimates what msg costs a request. Reasoning is
// charged as the provider replays it: the signed or encrypted state the
// message's metadata holds, or, where plainReasoning, its text, at the larger
// of the two. A provider that sends neither back is charged nothing for it:
// charging it would compact the conversation early for tokens the provider
// never sees.
func estimateMessageTokensWith(msg messages.ChatMessage, plainReasoning bool) int {
	base := messages.EstimateMessageTokens(msg) - estimatedStringTokens(msg.Reasoning)
	plain := 0
	if plainReasoning {
		plain = estimatedStringTokens(msg.Reasoning)
	}
	return base + max(plain, reasoningReplayTokens(msg))
}

// reasoningReplayTokens estimates the reasoning a provider replays from msg's
// metadata: signed thinking blocks, encrypted reasoning items, thought
// signatures. Providers send either that or the plain reasoning, never both,
// so a message is charged the larger of what its provider replays.
func reasoningReplayTokens(msg messages.ChatMessage) int {
	total := 0
	for _, key := range reasoningReplayKeys {
		value, ok := msg.Metadata[key]
		if !ok || key == openai.ResponsesReasoningModelKey {
			continue
		}
		if n := jsonLength(value); n > 0 {
			total += (n + 2) / 3
		}
	}
	return total
}

// jsonLength is about how long value is encoded as JSON, without encoding
// it: estimates run over every message of every request, and replayed
// reasoning (signed thinking, encrypted items) is the bulk of a message's
// metadata. Escapes are not counted.
func jsonLength(value any) int {
	switch v := value.(type) {
	case nil:
		return 4
	case string:
		return len(v) + 2
	case json.RawMessage:
		return len(v)
	case []byte:
		return len(v)
	case bool, float64, float32, int, int64, int32:
		return 8
	case map[string]any:
		n := 2
		for key, item := range v {
			n += len(key) + 4 + jsonLength(item)
		}
		return n
	case []any:
		n := 2
		for _, item := range v {
			n += jsonLength(item) + 1
		}
		return n
	case []map[string]any:
		n := 2
		for _, item := range v {
			n += jsonLength(item) + 1
		}
		return n
	case []string:
		n := 2
		for _, item := range v {
			n += len(item) + 3
		}
		return n
	case map[string]string:
		n := 2
		for key, item := range v {
			n += len(key) + len(item) + 6
		}
		return n
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(raw)
}

// estimateToolSchemaTokens estimates the request overhead of tool schemas,
// which share the model's context with the projected messages but are not part
// of them. A request's estimated size includes them.
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

// estimatedStringTokens and estimatedJSONTokens are thin wrappers over the
// shared rates defined once in the messages package.
func estimatedStringTokens(s string) int {
	return messages.EstimatedStringTokens(s)
}

func estimatedJSONTokens(s string) int {
	return messages.EstimatedJSONTokens(s)
}
