package messages

import (
	"strings"
	"testing"
)

func TestValidateEncodedImageBudgetIncludesDataURLs(t *testing.T) {
	history := []ChatMessage{{
		Role: MessageRoleUser,
		Parts: []ContentPart{
			{Type: "image_base64", ImageData: strings.Repeat("A", 8<<20)},
			{Type: "image_url", ImageURL: "data:image/png;base64," + strings.Repeat("B", (8<<20)+1)},
		},
	}}
	if err := ValidateEncodedImageBudget(history); err == nil || !strings.Contains(err.Error(), "portable limit is 16 MiB") {
		t.Fatalf("aggregate encoded-image overflow = %v", err)
	}
}
