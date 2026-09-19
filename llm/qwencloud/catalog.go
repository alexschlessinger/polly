package qwencloud

import (
	"context"
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// ListModels reads the compatible model listing and filters named lookups.
// Missing capabilities remain unknown rather than inferred from model names.
func ListModels(ctx context.Context, client *http.Client, target contract.ModelTarget) (contract.ModelCatalog, error) {
	return openai.ListCompatibleModels(ctx, client, target, catalog.BaseModel, false)
}
