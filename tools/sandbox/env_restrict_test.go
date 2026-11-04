package sandbox

import (
	"slices"
	"testing"
)

func TestRestrictAllowEnv(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cfg         Config
		allow, want []string
		fail        bool
	}{
		{name: "absent restriction", cfg: Config{AllowEnv: []string{"PATH"}}, want: []string{"PATH"}},
		{name: "default narrows", allow: []string{"PATH"}, want: []string{"PATH"}},
		{name: "intersection", cfg: Config{AllowEnv: []string{"PATH", "KEEP"}}, allow: []string{"PATH", "OTHER"}, want: []string{"PATH"}},
		{name: "disjoint", cfg: Config{AllowEnv: []string{"PATH"}}, allow: []string{"OTHER"}, fail: true},
		{name: "sensitive only", allow: []string{"REVIEW_API_TOKEN"}, fail: true},
		{name: "mixed sensitivity", allow: []string{"PATH", "REVIEW_API_TOKEN", "aws_region", "Review_Api_Token"}, want: []string{"PATH"}},
		{name: "exact case allow", cfg: Config{AllowEnv: []string{"PATH"}}, allow: []string{"Path"}, fail: true},
		{name: "exact case pass", cfg: Config{PassEnv: []string{"REVIEW_API_TOKEN"}}, allow: []string{"review_api_token"}, fail: true},
		{name: "authorized sensitive", cfg: Config{PassEnv: []string{"REVIEW_API_TOKEN"}}, allow: []string{"REVIEW_API_TOKEN"}, want: []string{"REVIEW_API_TOKEN"}},
		{name: "strict ignores pass", cfg: Config{AllowEnv: []string{"PATH"}, PassEnv: []string{"REVIEW_API_TOKEN"}}, allow: []string{"REVIEW_API_TOKEN"}, fail: true},
		{name: "absent ambient name", allow: []string{"ABSENT_BUT_AUTHORIZED_FIXTURE"}, want: []string{"ABSENT_BUT_AUTHORIZED_FIXTURE"}},
		{name: "explicit env is not ambient grant", cfg: Config{Env: map[string]string{"REVIEW_API_TOKEN": "configured"}}, allow: []string{"REVIEW_API_TOKEN"}, fail: true},
		{name: "invalid empty", allow: []string{""}, fail: true},
		{name: "invalid assignment", allow: []string{"PATH=bad"}, fail: true},
		{name: "invalid nul", allow: []string{"PATH\x00"}, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.RestrictAllowEnv(tc.allow)
			if (err != nil) != tc.fail {
				t.Fatalf("RestrictAllowEnv = %+v, %v; want failure %v", got, err, tc.fail)
			}
			if err == nil && !slices.Equal(got.AllowEnv, tc.want) {
				t.Fatalf("AllowEnv = %v, want %v", got.AllowEnv, tc.want)
			}
		})
	}
}

func TestRestrictAllowEnvPreservesExplicitPolicyAndMasks(t *testing.T) {
	cfg := Config{AllowEnv: []string{"PATH", "OTHER"}, Env: map[string]string{"POLICY_ONLY": "configured"}, DenyPaths: []string{"/masked"}, PrivateHome: true}
	got, err := cfg.RestrictAllowEnv([]string{"PATH", "REVIEW_API_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	filtered, _ := filterEnv([]string{"PATH=fixture", "OTHER=ambient", "REVIEW_API_TOKEN=secret"}, got.AllowEnv, got.PassEnv)
	if !slices.Equal(filtered, []string{"PATH=fixture"}) {
		t.Fatalf("filtered = %v", filtered)
	}
	if got.Env["POLICY_ONLY"] != "configured" || !got.PrivateHome || !slices.Equal(got.DenyPaths, cfg.DenyPaths) {
		t.Fatalf("unrelated policy lost: %+v", got)
	}
	got.AllowEnv[0] = "CHANGED"
	if cfg.AllowEnv[0] != "PATH" {
		t.Fatal("intersection aliases caller allowlist")
	}
}
