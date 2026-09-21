package sandbox

import (
	"encoding/json"
	"fmt"
)

// Declaration is a tool's own "sandbox" entry as its metadata declares it:
// absent, null or true ask for the base policy, an object adds overrides,
// and false opts out of sandboxing. It decodes leniently, so a config file
// with one unsupported entry still loads and the error surfaces when that
// tool is loaded, and it encodes back as declared.
type Declaration struct {
	raw    json.RawMessage
	config *Config
	optOut bool
	err    error
}

// DeclareConfig is the declaration of the overrides cfg.
func DeclareConfig(cfg Config) Declaration { return Declaration{config: &cfg} }

// ParseConfig parses a raw "sandbox" entry into its overrides: nil for an
// absent entry or an opt-out, an empty config for true, an error for an
// unsupported value.
func ParseConfig(raw json.RawMessage) (*Config, error) {
	config, _, err := parseDeclaration(raw)
	return config, err
}

// OptOut reports a declaration of false.
func (d Declaration) OptOut() bool { return d.optOut }

// Config returns the declared overrides as ParseConfig does.
func (d Declaration) Config() (*Config, error) { return d.config, d.err }

// IsZero reports an absent declaration, which omitzero leaves out.
func (d Declaration) IsZero() bool {
	return d.raw == nil && d.config == nil && !d.optOut && d.err == nil
}

func (d *Declaration) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*d = Declaration{}
		return nil
	}
	*d = Declaration{raw: append(json.RawMessage(nil), raw...)}
	d.config, d.optOut, d.err = parseDeclaration(raw)
	return nil
}

// MarshalJSON writes the entry back as declared, or the overrides of a
// declaration made in code.
func (d Declaration) MarshalJSON() ([]byte, error) {
	switch {
	case d.raw != nil:
		return d.raw, nil
	case d.optOut:
		return []byte("false"), nil
	case d.config != nil:
		return json.Marshal(d.config)
	}
	return []byte("null"), nil
}

func parseDeclaration(raw []byte) (config *Config, optOut bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		if b {
			return &Config{}, false, nil
		}
		return nil, true, nil
	}
	var c Config
	if json.Unmarshal(raw, &c) == nil {
		return &c, false, nil
	}
	return nil, false, fmt.Errorf("unsupported sandbox value: %s (must be true, false, or an object)", string(raw))
}
