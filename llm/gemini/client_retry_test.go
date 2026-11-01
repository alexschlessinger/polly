package gemini

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAPIErrorRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want bool
	}{
		{"nil", nil, false},
		{"unknown", &APIError{Message: "refused"}, false},
		{"bad request", &APIError{Code: 400}, false},
		{"authentication", &APIError{Status: "UNAUTHENTICATED"}, false},
		{"auth wins over status", &APIError{Code: 401, Status: "RESOURCE_EXHAUSTED"}, false},
		{"quota status", &APIError{Code: 429, Status: "QUOTA_EXCEEDED"}, false},
		{"billing", &APIError{Code: 429, Status: "BILLING_DISABLED"}, false},
		{"invalid request", &APIError{Code: 503, Status: "INVALID_ARGUMENT"}, false},
		{"rate code", &APIError{Code: 429}, true},
		{"overload code", &APIError{Code: 503}, true},
		{"timeout", &APIError{Code: 408}, true},
		{"conflict", &APIError{Code: 409}, true},
		{"rate stream", &APIError{Status: "RESOURCE_EXHAUSTED"}, true},
		{"overload stream", &APIError{Status: "UNAVAILABLE"}, true},
		{"internal stream", &APIError{Status: "INTERNAL"}, true},
		{"unknown details", &APIError{Code: 429, Details: json.RawMessage(`"unknown"`)}, true},
		{"daily quota", &APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Details: json.RawMessage(`[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier","quotaValue":"20"}]}]`)}, false},
		{"minute quota", &APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Details: json.RawMessage(`[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinute","quotaValue":"20"}]}]`)}, true},
		{"zero quota", &APIError{Code: 429, Details: json.RawMessage(`[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaValue":"0"}]}]`)}, false},
		{"zero numeric quota", &APIError{Code: 429, Details: json.RawMessage(`[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaValue":0}]}]`)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.want {
				t.Fatalf("Retryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestErrorFromResponsePreservesStatusWhenEnvelopeOmitsCode(t *testing.T) {
	resp := &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`))}
	defer resp.Body.Close()
	err := errorFromResponse(resp).(*APIError)
	if err.Code != 503 || !err.Retryable() {
		t.Fatalf("error lost HTTP status: %+v", err)
	}
}
