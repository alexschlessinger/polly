package anthropic

import (
	"context"
	"net/http"
	"slices"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListModels reads the Anthropic model catalog, following after_id cursors,
// or one model's record from /models/{id} when t.Model is set.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	path := t.BaseURL + "/models"
	if t.Model != "" {
		path += "/" + catalog.EscapeModelPath(t.Model)
	}
	headers := func(r *http.Request) {
		if t.APIKey != "" {
			r.Header.Set("x-api-key", t.APIKey)
		}
		r.Header.Set("anthropic-version", apiVersion)
	}
	var listing catalog.Listing
	err := catalog.Walk(func(token string) (string, error) {
		raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, catalog.PageQuery(path, "after_id", token), nil, headers)
		if err != nil {
			return "", err
		}
		if err := listing.AddPage(raw, "data", t.Model, decodeModel); err != nil {
			return "", err
		}
		return catalog.NextPage(raw)
	})
	return listing.Catalog(t.Model, err)
}

// decodeModel reads a catalog record: the display name, the capabilities
// object's tool, structured-output, thinking and image support, and the
// supported effort levels, complete only when every level is declared.
func decodeModel(r map[string]any) contract.ModelInfo {
	info := catalog.BaseModel(r)
	info.Name = catalog.Str(r["display_name"])
	info.Chat = catalog.Truth(true)
	caps := catalog.Obj(r["capabilities"])
	info.Tools = catalog.BoolPtr(catalog.Obj(caps["tool_use"])["supported"])
	info.StructuredOutput = catalog.BoolPtr(catalog.Obj(caps["structured_outputs"])["supported"])
	info.Reasoning = catalog.BoolPtr(catalog.Obj(caps["thinking"])["supported"])
	info.ReasoningOptions = catalog.Obj(caps["thinking"])
	if vision := catalog.BoolPtr(catalog.Obj(caps["image_input"])["supported"]); vision != nil {
		info.InputModalities = []string{"text"}
		if *vision {
			info.InputModalities = append(info.InputModalities, "image")
		}
	}
	effort := catalog.Obj(caps["effort"])
	for k, v := range effort {
		if k != "supported" && catalog.Bool(catalog.Obj(v)["supported"]) {
			info.ReasoningEfforts = append(info.ReasoningEfforts, k)
		}
	}
	slices.Sort(info.ReasoningEfforts)
	info.ReasoningEffortsComplete = catalog.BoolPtr(effort["supported"]) != nil
	for _, k := range []string{"low", "medium", "high", "xhigh", "max"} {
		if catalog.BoolPtr(catalog.Obj(effort[k])["supported"]) == nil {
			info.ReasoningEffortsComplete = false
		}
	}
	if info.ReasoningOptions == nil {
		info.ReasoningOptions = map[string]any{}
	}
	if effort != nil {
		info.ReasoningOptions["effort"] = effort
	}
	return info
}
