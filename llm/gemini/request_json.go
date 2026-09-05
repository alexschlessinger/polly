package gemini

import "encoding/json"

// MarshalJSON substitutes outgoing replay payloads without decoding images or
// rebuilding argument maps. The HTTP client calls this directly so each body
// is encoded once, rather than allocating an encoded buffer per content part.
func (r GenerateContentRequest) MarshalJSON() ([]byte, error) {
	type wireRequest GenerateContentRequest
	contents := make([]any, len(r.Contents))
	for i, content := range r.Contents {
		contents[i] = contentForWire(content)
	}
	return json.Marshal(struct {
		*wireRequest
		Contents          []any `json:"contents,omitempty"`
		SystemInstruction any   `json:"systemInstruction,omitempty"`
	}{(*wireRequest)(&r), contents, contentForWire(r.SystemInstruction)})
}

func contentForWire(content *Content) any {
	if content == nil {
		return nil
	}
	var parts []any
	for i, part := range content.Parts {
		if part == nil {
			if parts != nil {
				parts = append(parts, nil)
			}
			continue
		}
		hasReplay := part.InlineData != nil && part.InlineData.base64Data != nil ||
			part.FunctionCall != nil && part.FunctionCall.argsJSON != nil ||
			part.FunctionResponse != nil && part.FunctionResponse.responseJSON != nil
		if !hasReplay {
			if parts != nil {
				parts = append(parts, part)
			}
			continue
		}
		if parts == nil {
			parts = make([]any, 0, len(content.Parts))
			for _, previous := range content.Parts[:i] {
				parts = append(parts, previous)
			}
		}
		wire := struct {
			*Part
			InlineData       any `json:"inlineData,omitempty"`
			FunctionCall     any `json:"functionCall,omitempty"`
			FunctionResponse any `json:"functionResponse,omitempty"`
		}{Part: part}
		if blob := part.InlineData; blob != nil {
			wire.InlineData = blob
			if blob.base64Data != nil {
				wire.InlineData = struct {
					MIMEType string `json:"mimeType,omitempty"`
					Data     string `json:"data,omitempty"`
				}{blob.MIMEType, *blob.base64Data}
			}
		}
		if call := part.FunctionCall; call != nil {
			wire.FunctionCall = call
			if call.argsJSON != nil {
				wire.FunctionCall = struct {
					ID   string          `json:"id,omitempty"`
					Name string          `json:"name,omitempty"`
					Args json.RawMessage `json:"args,omitempty"`
				}{call.ID, call.Name, call.argsJSON}
			}
		}
		if result := part.FunctionResponse; result != nil {
			wire.FunctionResponse = result
			if result.responseJSON != nil {
				wire.FunctionResponse = struct {
					ID       string          `json:"id,omitempty"`
					Name     string          `json:"name,omitempty"`
					Response json.RawMessage `json:"response,omitempty"`
				}{result.ID, result.Name, result.responseJSON}
			}
		}
		parts = append(parts, wire)
	}
	if parts == nil {
		return content
	}
	return struct {
		*Content
		Parts []any `json:"parts,omitempty"`
	}{content, parts}
}
