package rex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ClientInfo describes a connected Rex client application or CLI.
type ClientInfo struct {
	Kind     string `json:"kind,omitempty"`
	Platform string `json:"platform,omitempty"`
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Label    string `json:"label,omitempty"`
}

// ConnectedClient is one entry from client.list or client.inspect.
type ConnectedClient struct {
	ClientID        string      `json:"client_id"`
	Info            *ClientInfo `json:"info,omitempty"`
	Principal       *Principal  `json:"principal,omitempty"`
	ConnectedAt     string      `json:"connected_at,omitempty"`
	ConnectionState string      `json:"connection_state,omitempty"`
	SessionIDs      []string    `json:"session_ids,omitempty"`
}

// ActionInfo describes an action offered by the server, a block, or a client app.
type ActionInfo struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Args        json.RawMessage `json:"args,omitempty"`
}

// ClientDetails is returned by client.inspect.
type ClientDetails struct {
	Client  ConnectedClient `json:"client"`
	Actions []ActionInfo    `json:"actions,omitempty"`
}

// ListClients lists clients connected to the Rex server, optionally filtered
// by session ID and/or client kinds (such as "app" or "cli").
func (c *Client) ListClients(ctx context.Context, session string, kinds ...string) ([]ConnectedClient, error) {
	req := map[string]any{}
	if session != "" {
		req["session_id"] = session
	}
	if len(kinds) > 0 {
		req["kinds"] = kinds
	}
	var out struct {
		Clients []ConnectedClient `json:"clients"`
	}
	if err := c.CallSession(ctx, "", "client.list", req, &out); err != nil {
		return nil, err
	}
	return out.Clients, nil
}

// InspectClient returns details and available UI actions for one connected client.
func (c *Client) InspectClient(ctx context.Context, clientID string) (*ClientDetails, error) {
	cid := clientID
	if cid == "" {
		cid = c.client
	}
	if cid == "" {
		return nil, fmt.Errorf("rex client.inspect: client_id is required")
	}
	var out ClientDetails
	if err := c.CallSession(ctx, "", "client.inspect", map[string]any{"client_id": cid}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListActions returns actions available on the server, or on clientID when
// non-empty (or when WithClient is configured on the Client).
func (c *Client) ListActions(ctx context.Context, clientID string) ([]ActionInfo, error) {
	target := c
	if clientID != "" {
		target = c.ForClient(clientID)
	}
	var raw json.RawMessage
	if err := target.runJSON(ctx, []string{"actions", "--json"}, nil, "actions", &raw); err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	// Rex may return either an array of ActionInfo or {"actions": [...]}.
	if strings.HasPrefix(trimmed, "[") {
		var list []ActionInfo
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("rex actions: decode array: %w", err)
		}
		return list, nil
	}
	var wrapped struct {
		Actions []ActionInfo `json:"actions"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("rex actions: decode object: %w", err)
	}
	return wrapped.Actions, nil
}

// PerformActionRequest configures client.perform to execute an action on a
// connected Rex app client (requires Remote Control to be enabled on the app).
type PerformActionRequest struct {
	ClientID  string         `json:"client_id"`
	Action    string         `json:"action"`
	Args      map[string]any `json:"args,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	BlockID   string         `json:"block_id,omitempty"`
	TimeoutMS int            `json:"timeout_ms,omitempty"`
}

// PerformClientAction asks a connected Rex app client to perform one of its
// actions and returns its JSON result (if any).
func (c *Client) PerformClientAction(ctx context.Context, req PerformActionRequest) (json.RawMessage, error) {
	if strings.TrimSpace(req.Action) == "" {
		return nil, fmt.Errorf("rex client.perform: action is required")
	}
	cid := req.ClientID
	if cid == "" {
		cid = c.client
	}
	if cid == "" {
		return nil, fmt.Errorf("rex client.perform: client_id is required")
	}
	payload := req
	payload.ClientID = cid
	raw, err := marshalCallArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("rex client.perform: encode args: %w", err)
	}
	return c.CallRaw(ctx, "", "client.perform", raw)
}

// InvokeActionRequest configures client.action to invoke a server/init.lua or
// block action.
type InvokeActionRequest struct {
	Action    string         `json:"action"`
	Args      map[string]any `json:"args,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	BlockID   string         `json:"block_id,omitempty"`
	Server    string         `json:"server,omitempty"`
}

// InvokeAction runs a server/init.lua or block action via client.action.
func (c *Client) InvokeAction(ctx context.Context, req InvokeActionRequest) (json.RawMessage, error) {
	if strings.TrimSpace(req.Action) == "" {
		return nil, fmt.Errorf("rex client.action: action is required")
	}
	payload := req
	if payload.SessionID == "" {
		payload.SessionID = c.EffectiveSession()
	}
	raw, err := marshalCallArgs(payload)
	if err != nil {
		return nil, fmt.Errorf("rex client.action: encode args: %w", err)
	}
	return c.CallRaw(ctx, "", "client.action", raw)
}
