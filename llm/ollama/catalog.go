package ollama

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListModels reads the local model list from /api/tags, or one model's
// details from /api/show when t.Model is set.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	if t.Model != "" {
		raw, err := catalog.FetchJSON(ctx, client, http.MethodPost, t.BaseURL+"/api/show", map[string]string{"model": t.Model}, catalog.Bearer(t.APIKey))
		if err != nil {
			return contract.ModelCatalog{}, err
		}
		info := decodeModel(raw)
		info.ID = t.Model
		return contract.ModelCatalog{Models: []contract.ModelInfo{info}}, nil
	}
	var listing catalog.Listing
	raw, err := catalog.FetchJSON(ctx, client, http.MethodGet, t.BaseURL+"/api/tags", nil, catalog.Bearer(t.APIKey))
	if err == nil {
		err = listing.AddPage(raw, "models", "", decodeModel)
	}
	return listing.Catalog("", err)
}

// decodeModel reads a tag or show record: the capability list, the
// architecture's context length, and any num_ctx runtime override.
func decodeModel(r map[string]any) contract.ModelInfo {
	info := catalog.BaseModel(r)
	info.ID = catalog.Str(r["name"])
	if info.ID == "" {
		info.ID = catalog.Str(r["model"])
	}
	if caps := catalog.Strings(r, "capabilities"); caps != nil {
		info.Chat = catalog.Truth(slices.Contains(caps, "completion"))
		info.Tools = catalog.Truth(slices.Contains(caps, "tools"))
		info.Reasoning = catalog.Truth(slices.Contains(caps, "thinking"))
		info.InputModalities = []string{"text"}
		if slices.Contains(caps, "vision") {
			info.InputModalities = append(info.InputModalities, "image")
		}
	}
	model := catalog.Obj(r["model_info"])
	info.ContextTokens = catalog.IntPtr(model[catalog.Str(model["general.architecture"])+".context_length"])
	for _, line := range strings.Split(catalog.Str(r["parameters"]), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "num_ctx" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				info.RuntimeContextTokens = &n
			}
		}
	}
	return info
}
