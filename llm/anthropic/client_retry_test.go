package anthropic

import "testing"

func TestAPIErrorRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{"nil", nil, false},
		{"unknown", &APIError{Message: "refused"}, false},
		{"bad request", &APIError{StatusCode: 400}, false},
		{"authentication", &APIError{Type: "authentication_error"}, false},
		{"auth wins over type", &APIError{StatusCode: 401, Type: "rate_limit_error"}, false},
		{"quota", &APIError{StatusCode: 429, Type: "insufficient_quota"}, false},
		{"billing", &APIError{StatusCode: 429, Type: "billing_error"}, false},
		{"invalid request", &APIError{StatusCode: 503, Type: "invalid_request_error"}, false},
		{"rate status", &APIError{StatusCode: 429}, true},
		{"overload status", &APIError{StatusCode: 529}, true},
		{"timeout", &APIError{StatusCode: 408}, true},
		{"conflict", &APIError{StatusCode: 409}, true},
		{"rate stream", &APIError{Type: "rate_limit_error"}, true},
		{"overload stream without status", &APIError{Type: "overloaded_error"}, true},
		{"overload stream with successful status", &APIError{StatusCode: 200, Type: "overloaded_error"}, true},
		{"internal stream", &APIError{Type: "api_error"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.want {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
