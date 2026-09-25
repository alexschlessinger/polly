package codex

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ProtocolVersion is the Codex CLI protocol release this package follows.
// The catalog request states it, since the backend lists a model only to
// clients at or above the version that first carried it.
const ProtocolVersion = "0.157.0"

// ListModels reads the models the account's plan serves, or one model's
// record when t.Model names it. The read needs the sign-in; without one
// there is no catalog. When the backend cannot be read, the models this
// package knows come back as a partial catalog together with the error,
// so a model can still be named while the read is retried later.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget, login contract.Login) (contract.ModelCatalog, error) {
	if login == nil {
		return contract.ModelCatalog{}, contract.ErrModelMetadataUnknown
	}
	cred, err := login.Credential(ctx)
	if err != nil {
		return contract.ModelCatalog{}, fmt.Errorf("%w: %v", contract.ErrModelMetadataUnknown, err)
	}
	base := strings.TrimRight(t.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if client == nil {
		client = &http.Client{}
	}
	uri := base + "/models?client_version=" + url.QueryEscape(ProtocolVersion)
	headers := func(r *http.Request) { setHeaders(r.Header, cred, "", "application/json") }
	var listing catalog.Listing
	raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, uri, nil, headers)
	if err == nil {
		err = listing.AddPage(raw, "models", t.Model, decodeModel)
	}
	if err != nil {
		slog.Debug("codex_catalog_unavailable", "error", err)
		fallback := knownCatalog(t.Model)
		if len(fallback.Models) == 0 {
			return fallback, contract.ErrModelMetadataUnknown
		}
		fallback.Partial = true
		return fallback, err
	}
	return listing.Catalog(t.Model, nil)
}

// decodeModel reads one backend record: the slug is the id, the plan
// serves every model as a chat model with tools and reasoning, and the
// record may state the context window, the output limit, and the
// reasoning levels the model takes.
func decodeModel(r map[string]any) contract.ModelInfo {
	info := catalog.BaseModel(r)
	if info.ID == "" {
		info.ID = catalog.Str(r["slug"])
	}
	if info.Name == "" {
		info.Name = catalog.Str(r["display_name"])
	}
	info.Chat, info.Tools, info.Reasoning = catalog.Truth(true), catalog.Truth(true), catalog.Truth(true)
	if info.InputModalities == nil {
		info.InputModalities = catalog.Strings(r, "input_modalities")
	}
	if info.InputModalities == nil {
		info.InputModalities = []string{"image", "text"}
	}
	if info.OutputModalities == nil {
		info.OutputModalities = []string{"text"}
	}
	if v := catalog.IntPtr(r["context_window"]); v != nil {
		info.ContextTokens = v
	}
	if v := catalog.IntPtr(r["max_output_tokens"]); v != nil {
		info.OutputTokens = v
	}
	if levels := reasoningLevels(r["supported_reasoning_levels"]); len(levels) > 0 {
		info.ReasoningEfforts, info.ReasoningEffortsComplete = levels, true
	}
	if d := catalog.Str(r["default_reasoning_level"]); d != "" {
		info.ReasoningDefaultEffort = &d
	}
	return info
}

// reasoningLevels reads the efforts a record lists, as bare words or as
// objects naming one.
func reasoningLevels(v any) []string {
	var out []string
	for _, entry := range catalog.Array(v) {
		switch x := entry.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			for _, key := range []string{"effort", "level", "id"} {
				if s := catalog.Str(x[key]); s != "" {
					out = append(out, s)
					break
				}
			}
		}
	}
	return out
}

// knownModel is a model the plan served when this package was written.
type knownModel struct {
	id, name        string
	context, output int
	textOnly        bool
}

// knownModels stand in when the backend's catalog cannot be read.
var knownModels = []knownModel{
	{id: "gpt-6-astra", name: "GPT-6 Astra", context: 272000, output: 128000},
	{id: "gpt-6-sol", name: "GPT-6 Sol", context: 272000, output: 128000},
	{id: "gpt-6-luna", name: "GPT-6 Luna", context: 272000, output: 128000},
	{id: "gpt-5.6-sol", name: "GPT-5.6 Sol", context: 272000, output: 128000},
	{id: "gpt-5.6-terra", name: "GPT-5.6 Terra", context: 272000, output: 128000},
	{id: "gpt-5.6-luna", name: "GPT-5.6 Luna", context: 272000, output: 128000},
	{id: "gpt-5.5", name: "GPT-5.5", context: 272000, output: 128000},
	{id: "gpt-5.3-codex-spark", name: "GPT-5.3 Codex Spark", context: 128000, output: 128000, textOnly: true},
}

// knownCatalog lists the known models, or the one named.
func knownCatalog(model string) contract.ModelCatalog {
	var cat contract.ModelCatalog
	for _, m := range knownModels {
		if model != "" && m.id != model {
			continue
		}
		context, output := m.context, m.output
		info := contract.ModelInfo{ID: m.id, Name: m.name, ModelCapabilities: contract.ModelCapabilities{
			Chat: catalog.Truth(true), Tools: catalog.Truth(true), Reasoning: catalog.Truth(true),
			InputModalities: []string{"image", "text"}, OutputModalities: []string{"text"},
			ContextTokens: &context, OutputTokens: &output,
		}}
		if m.textOnly {
			info.InputModalities = []string{"text"}
		}
		cat.Models = append(cat.Models, info)
	}
	return cat
}
