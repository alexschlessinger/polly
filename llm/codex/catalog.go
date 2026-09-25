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
	// A listing shows what the backend lists; a model it hides can still be
	// named.
	decode := decodeModel
	if t.Model == "" {
		decode = decodeListedModel
	}
	var listing catalog.Listing
	raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, uri, nil, headers)
	if err == nil {
		err = listing.AddPage(raw, "models", t.Model, decode)
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

// decodeListedModel is decodeModel for a listing: a record the backend
// hides decodes to nothing.
func decodeListedModel(r map[string]any) contract.ModelInfo {
	if v := catalog.Str(r["visibility"]); v != "" && v != "list" {
		return contract.ModelInfo{}
	}
	return decodeModel(r)
}

// decodeModel reads one backend record: the slug is the id, the plan
// serves every model as a chat model with tools and reasoning, and the
// record states the context window, the reasoning levels the model takes,
// and, for a model on its way out, when it retires and what replaces it.
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
	if upgrade := catalog.Obj(r["upgrade"]); upgrade != nil {
		if at := catalog.Str(upgrade["retirement_at"]); at != "" {
			info.Lifecycle["shutdown_date"] = at
		}
		if next := catalog.Str(upgrade["model"]); next != "" {
			info.Lifecycle["successor"] = next
		}
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

// knownModel is a model the backend listed when this package was written
// (2026-09-25); every one takes text and images in a 272k window.
type knownModel struct {
	id, name string
}

// knownModels stand in when the backend's catalog cannot be read.
var knownModels = []knownModel{
	{id: "gpt-6-astra", name: "GPT-6-Astra"},
	{id: "gpt-6-sol", name: "GPT-6-Sol"},
	{id: "gpt-6-luna", name: "GPT-6-Luna"},
	{id: "gpt-5.6-sol", name: "GPT-5.6-Sol"},
	{id: "gpt-5.6-terra", name: "GPT-5.6-Terra"},
	{id: "gpt-5.6-luna", name: "GPT-5.6-Luna"},
	{id: "gpt-5.5", name: "GPT-5.5"},
}

// knownContextTokens is the window every known model advertised.
const knownContextTokens = 272000

// knownCatalog lists the known models, or the one named.
func knownCatalog(model string) contract.ModelCatalog {
	var cat contract.ModelCatalog
	for _, m := range knownModels {
		if model != "" && m.id != model {
			continue
		}
		context := knownContextTokens
		cat.Models = append(cat.Models, contract.ModelInfo{ID: m.id, Name: m.name, ModelCapabilities: contract.ModelCapabilities{
			Chat: catalog.Truth(true), Tools: catalog.Truth(true), Reasoning: catalog.Truth(true),
			InputModalities: []string{"image", "text"}, OutputModalities: []string{"text"},
			ContextTokens: &context,
		}})
	}
	return cat
}
