package gemini

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListModels reads the Gemini model catalog, following pageTokens, or one
// model's record when t.Model is set. Model ids are used without the
// "models/" resource prefix.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	model := strings.TrimPrefix(t.Model, "models/")
	path := t.BaseURL + "/models"
	if model != "" {
		path += "/" + catalog.EscapeModelPath(model)
	}
	headers := func(r *http.Request) {
		if t.APIKey != "" {
			r.Header.Set("x-goog-api-key", t.APIKey)
		}
	}
	var listing catalog.Listing
	err := catalog.Walk(func(token string) (string, error) {
		raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, catalog.PageQuery(path, "pageToken", token), nil, headers)
		if err != nil {
			return "", err
		}
		if err := listing.AddPage(raw, "models", model, decodeModel); err != nil {
			return "", err
		}
		return catalog.NextPage(raw)
	})
	return listing.Catalog(model, err)
}

// decodeModel reads a catalog record: the resource name minus its prefix,
// token limits, thinking support, sampling defaults, and whether the model
// generates content at all.
func decodeModel(r map[string]any) contract.ModelInfo {
	info := catalog.BaseModel(r)
	info.ID = strings.TrimPrefix(catalog.Str(r["name"]), "models/")
	info.Name = catalog.Str(r["displayName"])
	info.InputTokens = catalog.IntPtr(r["inputTokenLimit"])
	info.OutputTokens = catalog.IntPtr(r["outputTokenLimit"])
	info.Reasoning = catalog.BoolPtr(r["thinking"])
	info.Sampling = catalog.AdvertisedFields(r, "temperature", "maxTemperature", "topP", "topK")
	if methods := catalog.Strings(r, "supportedGenerationMethods"); methods != nil {
		info.Chat = catalog.Truth(slices.Contains(methods, "generateContent"))
	}
	return info
}
