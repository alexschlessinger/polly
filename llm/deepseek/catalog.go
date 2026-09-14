package deepseek

import (
	"context"
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// ListModels reads the DeepSeek model listing. There is no per-model record,
// so a named lookup filters the listing by id and an alias is unknown.
func ListModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	return openai.ListCompatibleModels(ctx, client, t, catalog.BaseModel, false)
}
