package rex

import (
	"context"
	"encoding/json"
	"fmt"
)

// Principal describes how a Rex server authenticated a peer or client.
type Principal struct {
	Kind      string `json:"kind,omitempty"`
	Transport string `json:"transport,omitempty"`
	Display   string `json:"display,omitempty"`
	UID       int    `json:"uid,omitempty"`
	GID       int    `json:"gid,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

// WhoamiInfo is returned by "rex whoami --json".
type WhoamiInfo struct {
	Account      string          `json:"account,omitempty"`
	APIRevision  int             `json:"api_revision,omitempty"`
	BlockID      string          `json:"block_id,omitempty"`
	Endpoint     string          `json:"endpoint,omitempty"`
	ExitEmpty    bool            `json:"exit_empty,omitempty"`
	HostID       string          `json:"host_id,omitempty"`
	InstanceID   string          `json:"instance_id,omitempty"`
	LogPath      string          `json:"log_path,omitempty"`
	PID          int             `json:"pid,omitempty"`
	Principal    *Principal      `json:"principal,omitempty"`
	RexVersion   string          `json:"rex_version,omitempty"`
	SessionCount int             `json:"session_count,omitempty"`
	SessionID    string          `json:"session_id,omitempty"`
	StartedAt    string          `json:"started_at,omitempty"`
	WindowID     string          `json:"window_id,omitempty"`
	Tailscale    json.RawMessage `json:"tailscale,omitempty"`
}

// VersionInfo is returned by server.version.
type VersionInfo struct {
	RexVersion     string `json:"rex_version"`
	Platform       string `json:"platform,omitempty"`
	Commit         string `json:"commit,omitempty"`
	BuiltAt        string `json:"built_at,omitempty"`
	Dirty          bool   `json:"dirty,omitempty"`
	APIRevision    int    `json:"api_revision,omitempty"`
	APIMinRevision int    `json:"api_min_revision,omitempty"`
}

// ServerStatus is returned by server.status.
type ServerStatus struct {
	PID            int             `json:"pid,omitempty"`
	InstanceID     string          `json:"instance_id,omitempty"`
	HostID         string          `json:"host_id,omitempty"`
	StartedAt      string          `json:"started_at,omitempty"`
	ExitEmpty      bool            `json:"exit_empty,omitempty"`
	SessionCount   int             `json:"session_count,omitempty"`
	APIRevision    int             `json:"api_revision,omitempty"`
	APIMinRevision int             `json:"api_min_revision,omitempty"`
	Listeners      json.RawMessage `json:"listeners,omitempty"`
	Tailscale      json.RawMessage `json:"tailscale,omitempty"`
	Config         json.RawMessage `json:"config,omitempty"`
}

// BlockFlavor describes one flavor offered by a block creator.
type BlockFlavor struct {
	Flavor        string          `json:"flavor"`
	Title         string          `json:"title,omitempty"`
	Description   string          `json:"description,omitempty"`
	OptionsSchema json.RawMessage `json:"options_schema,omitempty"`
}

// BlockCreator describes a registered block creator on the server.
type BlockCreator struct {
	Name    string        `json:"name"`
	Flavors []BlockFlavor `json:"flavors"`
}

// BlockTypesResponse is returned by block_creator.list.
type BlockTypesResponse struct {
	Creators []BlockCreator `json:"creators"`
}

// SessionSummary is an entry in session.list.
type SessionSummary struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label,omitempty"`
}

// InitialWindowSpec describes a window to create atomically with session.create.
type InitialWindowSpec struct {
	WindowLabel string  `json:"window_label,omitempty"`
	Layout      *Layout `json:"layout"`
}

// CreateSessionRequest configures session.create.
type CreateSessionRequest struct {
	Label          string              `json:"label,omitempty"`
	InitialWindows []InitialWindowSpec `json:"initial_windows,omitempty"`
}

// WindowCreateResult is returned by session.new_window and session.create.
type WindowCreateResult struct {
	WindowID string   `json:"window_id"`
	BlockIDs []string `json:"block_ids"`
	Revision int      `json:"revision"`
}

// CreateSessionResponse is returned by session.create.
type CreateSessionResponse struct {
	SessionID      string               `json:"session_id"`
	InitialWindows []WindowCreateResult `json:"initial_windows,omitempty"`
}

// BlockSummary describes one block in session.view or session.list_blocks.
type BlockSummary struct {
	BlockID     string `json:"block_id"`
	Label       string `json:"label,omitempty"`
	CreatorName string `json:"creator_name,omitempty"`
	Flavor      string `json:"flavor,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Placement   string `json:"placement,omitempty"`
	WindowID    string `json:"window_id,omitempty"`
}

// LayoutNodeView is a live layout node returned by session.view.
type LayoutNodeView struct {
	Rect    Rect           `json:"rect,omitempty"`
	BlockID string         `json:"block_id,omitempty"`
	Split   *SplitNodeView `json:"split,omitempty"`
}

// SplitNodeView is a split branch inside LayoutNodeView.
type SplitNodeView struct {
	SplitID   string          `json:"split_id"`
	Direction string          `json:"direction"`
	Ratio     float64         `json:"ratio"`
	Before    *LayoutNodeView `json:"before,omitempty"`
	After     *LayoutNodeView `json:"after,omitempty"`
}

// LayerView is a tiled or floating layer inside WindowView.
type LayerView struct {
	LayerID string          `json:"layer_id"`
	Kind    string          `json:"kind"`
	State   string          `json:"state,omitempty"`
	Bounds  Rect            `json:"bounds,omitempty"`
	Layout  *LayoutNodeView `json:"layout,omitempty"`
	Blocks  []BlockSummary  `json:"blocks,omitempty"`
}

// WindowView is one tab inside SessionView.
type WindowView struct {
	WindowID       string      `json:"window_id"`
	Label          string      `json:"label,omitempty"`
	FocusedBlockID string      `json:"focused_block_id,omitempty"`
	ZoomedBlockID  string      `json:"zoomed_block_id,omitempty"`
	Layers         []LayerView `json:"layers,omitempty"`
}

// SessionView is the complete snapshot returned by session.view.
type SessionView struct {
	Revision       int          `json:"revision"`
	SessionID      string       `json:"session_id"`
	Label          string       `json:"label,omitempty"`
	ActiveWindowID string       `json:"active_window_id,omitempty"`
	Windows        []WindowView `json:"windows,omitempty"`
}

// ActiveWindow returns the window matching ActiveWindowID, or nil if none matches.
func (s *SessionView) ActiveWindow() *WindowView {
	if s == nil {
		return nil
	}
	for i := range s.Windows {
		if s.Windows[i].WindowID == s.ActiveWindowID {
			return &s.Windows[i]
		}
	}
	return nil
}

// FocusedBlockID returns the focused block ID in the active window, or "".
func (s *SessionView) FocusedBlockID() string {
	if w := s.ActiveWindow(); w != nil {
		return w.FocusedBlockID
	}
	return ""
}

// FindBlock searches every window and layer for a block matching blockID or label.
func (s *SessionView) FindBlock(idOrLabel string) *BlockSummary {
	if s == nil || idOrLabel == "" {
		return nil
	}
	for wi := range s.Windows {
		for li := range s.Windows[wi].Layers {
			for bi := range s.Windows[wi].Layers[li].Blocks {
				b := &s.Windows[wi].Layers[li].Blocks[bi]
				if b.BlockID == idOrLabel || b.Label == idOrLabel {
					return b
				}
			}
		}
	}
	return nil
}

// BlockDetails is returned by session.describe_block.
type BlockDetails struct {
	Block BlockSummary    `json:"block"`
	API   json.RawMessage `json:"api,omitempty"`
}

// NewWindowRequest configures session.new_window.
type NewWindowRequest struct {
	SessionID   string  `json:"session_id,omitempty"`
	WindowLabel string  `json:"window_label,omitempty"`
	Layout      *Layout `json:"layout"`
	Focus       *bool   `json:"focus,omitempty"`
}

// Whoami returns connection, server, and ambient session/window/block details.
func (c *Client) Whoami(ctx context.Context) (*WhoamiInfo, error) {
	var out WhoamiInfo
	if err := c.runJSON(ctx, []string{"whoami", "--json"}, nil, "whoami", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Version returns the connected server's version and API revision.
func (c *Client) Version(ctx context.Context) (*VersionInfo, error) {
	var out VersionInfo
	if err := c.CallSession(ctx, "", "server.version", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Status returns the connected server's status snapshot.
func (c *Client) Status(ctx context.Context) (*ServerStatus, error) {
	var out ServerStatus
	if err := c.CallSession(ctx, "", "server.status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BlockTypes lists every block creator and flavor available on the server.
func (c *Client) BlockTypes(ctx context.Context) (*BlockTypesResponse, error) {
	var out BlockTypesResponse
	if err := c.CallSession(ctx, "", "block_creator.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSessions returns every session on the server.
func (c *Client) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	var out struct {
		Sessions []SessionSummary `json:"sessions"`
	}
	if err := c.CallSession(ctx, "", "session.list", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// CreateSession creates a new session, optionally with initial windows.
func (c *Client) CreateSession(ctx context.Context, req CreateSessionRequest) (*CreateSessionResponse, error) {
	var out CreateSessionResponse
	if err := c.CallSession(ctx, "", "session.create", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InspectSession returns the full session layout and block view via session.view.
func (c *Client) InspectSession(ctx context.Context, session string) (*SessionView, error) {
	var out SessionView
	if err := c.CallSession(ctx, session, "session.view", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenameSession sets a session's display label.
func (c *Client) RenameSession(ctx context.Context, session, label string) (*RevisionResponse, error) {
	var out RevisionResponse
	if err := c.CallSession(ctx, session, "session.set_label", map[string]any{"label": label}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DestroySession closes a session and every window and block in it.
func (c *Client) DestroySession(ctx context.Context, session string) error {
	return c.CallSession(ctx, session, "session.destroy", nil, nil)
}

// ListBlocks lists every block in the session, including detached blocks.
func (c *Client) ListBlocks(ctx context.Context, session string) ([]BlockSummary, error) {
	var out struct {
		Blocks []BlockSummary `json:"blocks"`
	}
	if err := c.CallSession(ctx, session, "session.list_blocks", nil, &out); err != nil {
		return nil, err
	}
	return out.Blocks, nil
}

// DescribeBlock returns a block's metadata and supported methods/events/actions.
func (c *Client) DescribeBlock(ctx context.Context, session, blockID string) (*BlockDetails, error) {
	var out BlockDetails
	err := c.CallSession(ctx, session, "session.describe_block", map[string]any{
		"block_id": c.resolveBlock(blockID),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// NewWindow creates a new tab (window) with the specified Layout.
func (c *Client) NewWindow(ctx context.Context, req NewWindowRequest) (*WindowCreateResult, error) {
	if req.Layout == nil {
		return nil, fmt.Errorf("rex session.new_window: layout is required")
	}
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out WindowCreateResult
	if err := c.CallSession(ctx, sid, "session.new_window", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenameWindow updates a tab's display label.
func (c *Client) RenameWindow(ctx context.Context, session, windowID, label string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.set_window_label", map[string]any{
		"window_id": windowID,
		"label":     label,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseWindow closes a tab and all of its blocks.
func (c *Client) CloseWindow(ctx context.Context, session, windowID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.close_window", map[string]any{
		"window_id": windowID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FocusWindow makes windowID the active tab in its session.
func (c *Client) FocusWindow(ctx context.Context, session, windowID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.focus_window", map[string]any{
		"window_id": windowID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// MoveWindow reorders windowID ahead of beforeWindowID (or to the end when empty).
func (c *Client) MoveWindow(ctx context.Context, session, windowID, beforeWindowID string) (*RevisionResponse, error) {
	req := map[string]any{"window_id": windowID}
	if beforeWindowID != "" {
		req["before_window_id"] = beforeWindowID
	}
	var out RevisionResponse
	if err := c.CallSession(ctx, session, "session.move_window", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveTarget calls target.resolve with raw query parameters and returns the
// server's JSON response.
func (c *Client) ResolveTarget(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	raw, err := marshalCallArgs(req)
	if err != nil {
		return nil, fmt.Errorf("rex target.resolve: encode args: %w", err)
	}
	return c.CallRaw(ctx, "", "target.resolve", raw)
}
