package docker

import "encoding/json"

func encodeBody(body any) (json.RawMessage, error) {
	return json.Marshal(body)
}
