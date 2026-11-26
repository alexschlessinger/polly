package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LuaOptions configures "rex do -" Lua script execution.
type LuaOptions struct {
	SessionID string
	BlockID   string
	All       bool
	For       time.Duration
	FailFast  bool
	Action    string
	Args      map[string]any
}

// EvalLua runs a Lua script against the Rex server by piping script to
// "rex do -" and decodes its JSON return value into out (if non-nil).
func (c *Client) EvalLua(ctx context.Context, script string, opts LuaOptions, out any) error {
	raw, err := c.EvalLuaRaw(ctx, script, opts)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("rex do: decode json result: %w", err)
	}
	return nil
}

// EvalLuaRaw runs a Lua script against the Rex server via "rex do -" and
// returns its raw stdout bytes (JSON-encoded return value, or empty if the
// script returned nothing).
func (c *Client) EvalLuaRaw(ctx context.Context, script string, opts LuaOptions) (json.RawMessage, error) {
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("rex do: script is required")
	}
	subArgs := []string{"do"}
	if opts.All {
		subArgs = append(subArgs, "--all")
	} else if sid := c.resolveSession(opts.SessionID); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	if bid := c.resolveBlock(opts.BlockID); bid != "" && !opts.All {
		subArgs = append(subArgs, "-b", bid)
	}
	if opts.For > 0 {
		subArgs = append(subArgs, "--for", opts.For.String())
	}
	if opts.FailFast {
		subArgs = append(subArgs, "--fail-fast")
	}
	if opts.Action != "" {
		subArgs = append(subArgs, "--action", opts.Action)
	}
	if len(opts.Args) > 0 {
		encoded, err := json.Marshal(opts.Args)
		if err != nil {
			return nil, fmt.Errorf("rex do: encode args: %w", err)
		}
		subArgs = append(subArgs, "--args", string(encoded))
	}
	subArgs = append(subArgs, "-")

	res, err := c.runRaw(ctx, subArgs, []byte(script), "do")
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(res.Stdout), nil
}
