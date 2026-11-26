package rex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// InvokeSpec describes how to invoke a documented server or block method.
type InvokeSpec struct {
	Method string `json:"method"`
}

// MethodDoc is the server's self-documented schema for a single RPC method or
// block method returned by "rex api list --json" and "rex api describe --json".
type MethodDoc struct {
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	Invoke          *InvokeSpec     `json:"invoke,omitempty"`
	InputSchema     json.RawMessage `json:"input_schema,omitempty"`
	ArgsSchema      json.RawMessage `json:"args_schema,omitempty"`
	OutputSchema    json.RawMessage `json:"output_schema,omitempty"`
	ResultSchema    json.RawMessage `json:"result_schema,omitempty"`
	RequestExample  string          `json:"request_example,omitempty"`
	ResponseExample string          `json:"response_example,omitempty"`
	ValidationNotes []string        `json:"validation_notes,omitempty"`
}

// EffectiveInputSchema returns InputSchema for server methods or ArgsSchema for
// block methods.
func (m *MethodDoc) EffectiveInputSchema() json.RawMessage {
	if len(m.InputSchema) > 0 {
		return m.InputSchema
	}
	return m.ArgsSchema
}

// EffectiveOutputSchema returns OutputSchema or ResultSchema, whichever the
// server populated.
func (m *MethodDoc) EffectiveOutputSchema() json.RawMessage {
	if len(m.OutputSchema) > 0 {
		return m.OutputSchema
	}
	return m.ResultSchema
}

// BlockTypeDocs groups the documented methods exposed by one block creator.
type BlockTypeDocs struct {
	Creator string      `json:"creator"`
	Methods []MethodDoc `json:"methods"`
}

// HTTPPaths reports the server's documented HTTP endpoints when present.
type HTTPPaths struct {
	ControlPath  string `json:"control_path,omitempty"`
	DataPath     string `json:"data_path,omitempty"`
	HelpPath     string `json:"help_path,omitempty"`
	LLMsHTMLPath string `json:"llms_html_path,omitempty"`
	LLMsTxtPath  string `json:"llms_txt_path,omitempty"`
}

// APIList is the full server API discovery document returned by "rex api list --json".
type APIList struct {
	Methods    []MethodDoc     `json:"methods"`
	BlockTypes []BlockTypeDocs `json:"block_types,omitempty"`
	HTTP       *HTTPPaths      `json:"http,omitempty"`
}

// ListMethods fetches the complete self-documented method catalog from the
// connected Rex server.
func (c *Client) ListMethods(ctx context.Context) (*APIList, error) {
	var out APIList
	if err := c.runJSON(ctx, []string{"api", "list", "--json"}, nil, "api.list", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DescribeMethod fetches the detailed JSON-Schema documentation for one server
// or block method (for example "session.new_split" or
// "com.superlogical.terminal.format").
func (c *Client) DescribeMethod(ctx context.Context, method string) (*MethodDoc, error) {
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, fmt.Errorf("rex describe: method name is required")
	}
	var out MethodDoc
	if err := c.runJSON(ctx, []string{"api", "describe", method, "--json"}, nil, "api.describe:"+method, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Call invokes any Rex server method using the client's effective session (if
// any) and decodes the JSON response into out.
func (c *Client) Call(ctx context.Context, method string, args any, out any) error {
	return c.CallSession(ctx, "", method, args, out)
}

// CallSession invokes a Rex server method scoped to session (falling back to
// c.EffectiveSession() when session is empty). Passing args == nil sends "{}".
func (c *Client) CallSession(ctx context.Context, session, method string, args any, out any) error {
	payload, err := marshalCallArgs(args)
	if err != nil {
		return fmt.Errorf("rex %s: encode args: %w", method, err)
	}
	raw, err := c.CallRaw(ctx, session, method, payload)
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
		return fmt.Errorf("rex %s: decode response: %w", method, err)
	}
	return nil
}

// CallRaw invokes a Rex server method with a raw JSON argument object over
// stdin and returns the raw JSON response bytes.
func (c *Client) CallRaw(ctx context.Context, session, method string, args json.RawMessage) (json.RawMessage, error) {
	method = strings.TrimSpace(method)
	if method == "" {
		return nil, fmt.Errorf("rex api call: method name is required")
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 {
		trimmed = []byte("{}")
	}
	subArgs := []string{"api", "call"}
	if session != "" {
		subArgs = append(subArgs, "-s", session)
	} else if s := c.EffectiveSession(); s != "" && methodAcceptsSessionFlag(method) {
		subArgs = append(subArgs, "-s", s)
	}
	subArgs = append(subArgs, method, "-")
	res, err := c.runRaw(ctx, subArgs, trimmed, method)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(res.Stdout), nil
}

// CallBlock invokes a method on a specific block via "block.method" or a
// creator-qualified block method (such as "com.superlogical.terminal.format").
// When blockID is empty, c.EffectiveBlock() is used.
func (c *Client) CallBlock(ctx context.Context, session, blockID, creator, method string, args any, out any) error {
	method = strings.TrimSpace(method)
	if method == "" {
		return fmt.Errorf("rex block call: method name is required")
	}
	bid := c.resolveBlock(blockID)
	if bid == "" {
		return fmt.Errorf("rex block call %s: block_id is required", method)
	}
	sid := c.resolveSession(session)

	// When a creator is specified (or method is already creator-qualified),
	// call the qualified method directly via rex api call so Rex validates
	// against the block method's schema; otherwise dispatch through block.method.
	if creator != "" && !strings.HasPrefix(method, creator+".") {
		qualified := creator + "." + method
		req := map[string]any{"block_id": bid}
		if args != nil {
			req["args"] = args
		}
		return c.CallSession(ctx, sid, qualified, req, out)
	}
	if strings.Contains(method, ".") {
		req := map[string]any{"block_id": bid}
		if args != nil {
			req["args"] = args
		}
		return c.CallSession(ctx, sid, method, req, out)
	}

	req := map[string]any{
		"block_id": bid,
		"method":   method,
	}
	if args != nil {
		req["args"] = args
	}
	return c.CallSession(ctx, sid, "block.method", req, out)
}

func marshalCallArgs(args any) (json.RawMessage, error) {
	if args == nil {
		return json.RawMessage("{}"), nil
	}
	if raw, ok := args.(json.RawMessage); ok {
		if len(bytes.TrimSpace(raw)) == 0 {
			return json.RawMessage("{}"), nil
		}
		return raw, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return json.RawMessage("{}"), nil
	}
	return b, nil
}

// methodAcceptsSessionFlag reports whether rex api call -s SESSION should be
// passed for method. Server-global methods such as server.version, session.list,
// session.create, hosts.*, client.list, client.inspect, target.resolve, and
// block_creator.list have additionalProperties:false and reject session_id.
func methodAcceptsSessionFlag(method string) bool {
	if strings.HasPrefix(method, "block.") {
		return true
	}
	if strings.Count(method, ".") >= 2 {
		// Creator-qualified block methods use reverse-DNS prefixes such as
		// com.superlogical.terminal.format.
		return true
	}
	if strings.HasPrefix(method, "session.") {
		switch method {
		case "session.list", "session.create", "session.attach_all", "session.bootstrap":
			return false
		default:
			return true
		}
	}
	return false
}
