package openai

import (
	"context"
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListModels reads an OpenAI model listing, or one model's record from
// /models/{id} when t.Model is set.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	return ListCompatibleModels(ctx, client, t, catalog.BaseModel, true)
}

// ListCompatibleModels walks GET {base}/models of an OpenAI-compatible
// endpoint, decoding each "data" row with decode. When detail is set and
// t.Model names a model, /models/{id} is read instead and its single record
// is accepted whatever id it reports; otherwise a named lookup filters the
// listing by id. Pages continue through a nextPageToken.
func ListCompatibleModels(ctx context.Context, client *http.Client, t contract.ModelTarget, decode func(map[string]any) contract.ModelInfo, detail bool) (contract.ModelCatalog, error) {
	path := t.BaseURL + "/models"
	if detail && t.Model != "" {
		path += "/" + catalog.EscapeModelPath(t.Model)
	}
	var listing catalog.Listing
	err := catalog.Walk(func(token string) (string, error) {
		raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, catalog.PageQuery(path, "pageToken", token), nil, catalog.Bearer(t.APIKey))
		if err != nil {
			return "", err
		}
		if err := listing.AddPage(raw, "data", t.Model, decode); err != nil {
			return "", err
		}
		return catalog.NextPage(raw)
	})
	return listing.Catalog(t.Model, err)
}
