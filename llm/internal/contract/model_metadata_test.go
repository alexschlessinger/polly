package contract

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCatalogServiceTiersJSONRoundTrip(t *testing.T) {
	for _, tiers := range [][]string{nil, {}, {ServiceTierPriority, ServiceTierUltrafast}} {
		cat := ModelCatalog{Models: []ModelInfo{{ID: "model", ModelCapabilities: ModelCapabilities{ServiceTiers: tiers}}}}
		data, err := json.Marshal(cat)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ModelCatalog
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if got := decoded.Models[0].ServiceTiers; !reflect.DeepEqual(got, tiers) {
			t.Fatalf("round trip: got %#v, want %#v", got, tiers)
		}
	}
}

func TestRoutedServiceTiersComeFromEndpoints(t *testing.T) {
	info := ModelInfo{Routed: true, EndpointsComplete: true,
		ModelCapabilities: ModelCapabilities{ServiceTiers: []string{ServiceTierPriority, ServiceTierUltrafast}},
		Endpoints: []ModelEndpointInfo{
			{ID: "no-tiers", ModelCapabilities: ModelCapabilities{ServiceTiers: []string{}}},
			{ID: "unknown"},
		},
	}
	if tiers := info.EffectiveCapabilities("no-tiers").ServiceTiers; tiers == nil || len(tiers) != 0 {
		t.Fatalf("known-empty endpoint tiers = %#v", tiers)
	}
	for _, host := range []string{"unknown", ""} {
		if tiers := info.EffectiveCapabilities(host).ServiceTiers; tiers != nil {
			t.Fatalf("host %q inherited tiers %#v", host, tiers)
		}
	}
}

func TestMetadataUnknownZeroAndUnlimitedLimits(t *testing.T) {
	zero, finite := 0, 100
	for _, tc := range []struct {
		a, b      *int
		au, bu    bool
		want      *int
		unlimited bool
	}{
		{nil, nil, false, false, nil, false}, {&zero, &finite, false, false, &zero, false},
		{nil, &finite, true, false, &finite, false}, {nil, nil, true, true, nil, true},
		{nil, nil, true, false, nil, false},
	} {
		got, unlimited := commonModelLimit(tc.a, tc.b, tc.au, tc.bu)
		if !reflect.DeepEqual(got, tc.want) || unlimited != tc.unlimited {
			t.Fatalf("limit %v %v", got, unlimited)
		}
	}
	model := ModelInfo{Routed: true, EndpointsComplete: true, Endpoints: []ModelEndpointInfo{
		{ID: "a", ModelCapabilities: ModelCapabilities{UnlimitedLimits: map[string]bool{"contextTokens": true}}},
		{ID: "b", ModelCapabilities: ModelCapabilities{ContextTokens: &finite}},
	}}
	if got := model.EffectiveCapabilities("").ContextWindow(); got != finite {
		t.Fatalf("unlimited host lost finite sibling: %d", got)
	}
}

func TestExplicitModelWideLimitAppliesWithPartialHosts(t *testing.T) {
	n := 32000
	info := ModelInfo{Routed: true, LimitsApplyToAllRoutes: true, ModelCapabilities: ModelCapabilities{ContextTokens: &n}}
	if info.EffectiveCapabilities("").ContextWindow() != n {
		t.Fatal("explicit common model limit lost")
	}
	info.LimitsApplyToAllRoutes = false
	if info.EffectiveCapabilities("").ContextWindow() != 0 {
		t.Fatal("aggregate model limit became a guarantee")
	}
}
