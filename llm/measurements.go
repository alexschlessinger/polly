package llm

import (
	"encoding/json"
	"hash/fnv"
	"io"
	"math"
	"strconv"
	"sync"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// measurements are what a provider's counts said the messages of one
// conversation cost, by provider-visible form. A request that extends the
// last one sent, with the same tools, adds its appended messages to the
// prompt and nothing else, so the provider's count for it less the last
// count is what those messages cost; that is split among them in proportion
// to what they were priced at, and each is priced at its share wherever the
// same form recurs. A message in another form (a stub, a receipt, the
// content back inline) is a form of its own. The estimate stands until a
// form has been measured, so the measurements are as exact as the
// provider's counts and cover whatever the conversation has sent; the
// calibration ratio covers the rest. Counts are the provider's tokens; the
// projection prices in estimated tokens, so a count is scaled by the ratio
// its budget was sized by (see scaledCount).
type measurements struct {
	mu     sync.Mutex
	counts map[uint64]int
	// last is the last request sent, by form, with the tools it offered and
	// the input tokens the provider counted for it.
	last         []sentForm
	lastTools    string
	lastReported int
}

// sentForm is one projected message of a sent request: its form and the
// tokens the request priced it at, in the provider's count.
type sentForm struct {
	form   uint64
	tokens int
}

// scaledCount is a provider's count of n tokens in estimated tokens, at
// ratio provider tokens per estimated one; a ratio of zero is one.
func scaledCount(n int, ratio float64) int {
	if ratio <= 0 || ratio == 1 {
		return n
	}
	return max(1, int(math.Round(float64(n)/ratio)))
}

// count is what form was measured at, if it was.
func (m *measurements) count(form uint64) (int, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.counts[form]
	return n, ok
}

// learn keeps what the provider's count of a request, reported, said about
// the forms it carried with the given tools, and returns the forms it priced
// and their prices. A request that does not extend the last one sent with
// the same tools, or whose count differs from the last by less than nothing
// or by far more or less than the appended forms were priced at, prices
// nothing; it is the last request from then on either way.
func (m *measurements) learn(forms []sentForm, tools string, reported int) map[uint64]int {
	if m == nil || reported <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	last, lastTools, lastReported := m.last, m.lastTools, m.lastReported
	m.last, m.lastTools, m.lastReported = forms, tools, reported
	if last == nil || tools != lastTools || len(forms) <= len(last) {
		return nil
	}
	for i := range last {
		if forms[i].form != last[i].form {
			return nil
		}
	}
	delta := reported - lastReported
	appended := forms[len(last):]
	priced := 0
	for _, f := range appended {
		priced += f.tokens
	}
	if delta <= 0 || priced <= 0 || float64(delta) < float64(priced)/maxRatio || float64(delta) > float64(priced)*maxRatio {
		return nil
	}
	if m.counts == nil {
		m.counts = map[uint64]int{}
	}
	learned := make(map[uint64]int, len(appended))
	remaining := delta
	for i, f := range appended {
		share := delta * f.tokens / priced
		if i == len(appended)-1 {
			share = remaining
		}
		remaining -= share
		m.counts[f.form] = share
		learned[f.form] = share
	}
	return learned
}

// formFingerprint identifies a message's provider-visible form: what a
// provider is sent for it, so a count measured for the form prices it again
// wherever it recurs. Artifact parts are references the projection resolves
// or strips, never sent, and are left out.
func formFingerprint(msg messages.ChatMessage) uint64 {
	h := fnv.New64a()
	w := func(s string) {
		io.WriteString(h, s)
		h.Write([]byte{0})
	}
	w(msg.Role)
	text := msg.Content
	if msg.HasTextBlocks() {
		text = msg.ModelText()
	}
	w(text)
	w(msg.Reasoning)
	w(msg.ToolCallID)
	w(msg.ToolName)
	for _, call := range msg.ToolCalls {
		w(call.ID)
		w(call.Name)
		w(call.Arguments)
	}
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			w(part.Type)
			w(part.Text)
		case "image_base64":
			w(part.Type)
			w(part.MimeType)
			w(strconv.Itoa(len(part.ImageData)))
		case "image_url":
			w(part.Type)
			w(part.ImageURL)
		case "file":
			w(part.Type)
			w(part.FileName)
			w(part.MimeType)
			w(strconv.Itoa(len(part.Text) + len(part.ImageData)))
		}
	}
	for _, key := range reasoningReplayKeys {
		if value, ok := msg.Metadata[key]; ok {
			if raw, err := json.Marshal(value); err == nil {
				w(key)
				h.Write(raw)
				h.Write([]byte{0})
			}
		}
	}
	return h.Sum64()
}

// toolsFingerprint identifies the tools a request offers, by name and
// schema; requests offering different tools cannot be compared by count.
func toolsFingerprint(list []tools.Tool) string {
	if len(list) == 0 {
		return ""
	}
	h := fnv.New64a()
	for _, tool := range list {
		io.WriteString(h, tool.GetName())
		h.Write([]byte{0})
		if schema := tool.GetSchema(); schema != nil {
			if raw, err := json.Marshal(schema.Raw); err == nil {
				h.Write(raw)
			}
			if schema.Strict {
				h.Write([]byte{1})
			}
		}
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
