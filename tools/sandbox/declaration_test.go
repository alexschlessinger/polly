package sandbox

import (
	"encoding/json"
	"testing"
)

func TestDeclarationJSON(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		optOut  bool
		none    bool
		wantErr bool
		out     string // encoded form; "" is omitted
	}{
		{raw: `true`, out: `true`},
		{raw: `false`, optOut: true, none: true, out: `false`},
		{raw: `null`, none: true},
		{raw: `{"allowNetwork":true}`, out: `{"allowNetwork":true}`},
		{raw: `"yes"`, wantErr: true, none: true, out: `"yes"`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var holder struct {
				Sandbox Declaration `json:"sandbox,omitzero"`
			}
			if err := json.Unmarshal([]byte(`{"sandbox":`+tc.raw+`}`), &holder); err != nil {
				t.Fatalf("a declaration must decode leniently: %v", err)
			}
			cfg, err := holder.Sandbox.Config()
			if (err != nil) != tc.wantErr || (cfg == nil) != tc.none || holder.Sandbox.OptOut() != tc.optOut {
				t.Fatalf("Config() = %+v, %v; OptOut() = %v", cfg, err, holder.Sandbox.OptOut())
			}
			data, err := json.Marshal(holder)
			if err != nil {
				t.Fatal(err)
			}
			want := `{}`
			if tc.out != "" {
				want = `{"sandbox":` + tc.out + `}`
			}
			if string(data) != want {
				t.Fatalf("encoded %s, want %s", data, want)
			}
		})
	}
	data, err := json.Marshal(DeclareConfig(Config{DenyWrite: true, DenyPaths: []string{"/secret"}}))
	if err != nil {
		t.Fatal(err)
	}
	var declared Declaration
	if err := json.Unmarshal(data, &declared); err != nil {
		t.Fatal(err)
	}
	cfg, err := declared.Config()
	if err != nil || cfg == nil || !cfg.DenyWrite || len(cfg.DenyPaths) != 1 || declared.OptOut() {
		t.Fatalf("declared overrides did not round-trip: %+v %v", cfg, err)
	}
}

func TestNetworkAllowed(t *testing.T) {
	if err := NetworkAllowed(Config{}, "127.0.0.1"); err == nil {
		t.Fatal("network must be denied without allowNetwork")
	}
	if err := NetworkAllowed(Config{AllowNetwork: true}, "example.com"); err != nil {
		t.Fatal(err)
	}
	noDNS := Config{AllowNetwork: true, DenyDNS: true}
	for _, host := range []string{"example.com", "localhost", ""} {
		if err := NetworkAllowed(noDNS, host); err == nil {
			t.Fatalf("host %q must need DNS", host)
		}
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		if err := NetworkAllowed(noDNS, host); err != nil {
			t.Fatal(err)
		}
	}
}
