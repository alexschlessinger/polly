package openai

import (
	"context"
	"net/http"

	"github.com/alexschlessinger/pollytool/llm/internal/catalog"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ListHuggingFaceModels reads the Hugging Face router catalog, whose records
// carry each serving provider as an endpoint, or one model's record when
// t.Model is set.
func ListHuggingFaceModels(ctx context.Context, client *http.Client, t contract.ModelTarget) (contract.ModelCatalog, error) {
	return ListCompatibleModels(ctx, client, t, decodeHuggingFaceModel, true)
}

func decodeHuggingFaceModel(r map[string]any) contract.ModelInfo {
	info := catalog.RoutedModel(r)
	info.PricingUnit = "USD per million tokens"
	providers := catalog.Array(r["providers"])
	info.EndpointsComplete = providers != nil
	for _, value := range providers {
		info.Endpoints = append(info.Endpoints, decodeHuggingFaceEndpoint(catalog.Obj(value)))
	}
	info.Prices = catalog.Prices(info.Pricing, r, huggingFacePriceUnit)
	return info
}

func decodeHuggingFaceEndpoint(r map[string]any) contract.ModelEndpointInfo {
	e := catalog.Endpoint(r)
	e.PricingUnit = "USD per million tokens"
	e.Prices = catalog.Prices(e.Pricing, r, huggingFacePriceUnit)
	return e
}

func huggingFacePriceUnit(item string) string {
	if item == "input" || item == "output" {
		return "million tokens"
	}
	return ""
}
