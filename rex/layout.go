package rex

import (
	"context"
	"fmt"
)

// Common Rex block creators, flavors, split directions, sides, and layer states.
const (
	CreatorTerminal     = "com.superlogical.terminal"
	FlavorTerminalShell = "com.superlogical.terminal.shell"

	DirectionHorizontal = "horizontal"
	DirectionVertical   = "vertical"

	SideBefore = "before"
	SideAfter  = "after"

	FocusLeft  = "left"
	FocusRight = "right"
	FocusUp    = "up"
	FocusDown  = "down"

	LayerStateNormal    = "normal"
	LayerStateMinimized = "minimized"
)

// Rect is a normalized [x, y, w, h] rectangle in window-fraction coordinates.
type Rect [4]float64

// Layout is a recursive tree describing blocks to create in a window, split,
// or floating layer. Each node sets either Block or Split.
type Layout struct {
	Block *BlockSpec `json:"block,omitempty"`
	Split *SplitSpec `json:"split,omitempty"`
}

// BlockSpec describes one block leaf in a Layout.
type BlockSpec struct {
	Flavor  string `json:"flavor"`
	Label   string `json:"label,omitempty"`
	Options any    `json:"options,omitempty"`
}

// SplitSpec divides space horizontally or vertically between Before and After.
type SplitSpec struct {
	Direction string  `json:"direction"`
	Before    *Layout `json:"before"`
	After     *Layout `json:"after"`
	Ratio     float64 `json:"ratio,omitempty"`
}

// TerminalExitOptions controls whether a terminal block closes when its command exits.
type TerminalExitOptions struct {
	OnCompletion         *bool   `json:"on_completion,omitempty"`
	QuickExitThresholdMS *uint32 `json:"quick_exit_threshold_ms,omitempty"`
}

// TerminalSize specifies terminal dimensions in character cells and optional cell pixel sizes.
type TerminalSize struct {
	Columns      int  `json:"columns"`
	Rows         int  `json:"rows"`
	CellWidthPX  *int `json:"cell_width_px,omitempty"`
	CellHeightPX *int `json:"cell_height_px,omitempty"`
}

// TerminalTheme configures a terminal block's default colors.
type TerminalTheme struct {
	Background string   `json:"background,omitempty"`
	Foreground string   `json:"foreground,omitempty"`
	Cursor     string   `json:"cursor,omitempty"`
	Palette    []string `json:"palette,omitempty"`
	Scheme     string   `json:"scheme,omitempty"`
}

// TerminalOptions configures a com.superlogical.terminal.shell block.
type TerminalOptions struct {
	Command      []string             `json:"command,omitempty"`
	Cwd          string               `json:"cwd,omitempty"`
	Exit         *TerminalExitOptions `json:"exit,omitempty"`
	InitialInput string               `json:"initial_input,omitempty"`
	Interactive  bool                 `json:"interactive,omitempty"`
	Login        *bool                `json:"login,omitempty"`
	PathPrepend  []string             `json:"path_prepend,omitempty"`
	Shell        string               `json:"shell,omitempty"`
	Size         *TerminalSize        `json:"size,omitempty"`
	Theme        *TerminalTheme       `json:"theme,omitempty"`
}

// KeepOpen returns TerminalExitOptions that preserve the terminal pane after
// its command finishes.
func KeepOpen() *TerminalExitOptions {
	f := false
	return &TerminalExitOptions{OnCompletion: &f}
}

// BlockLayout returns a single-block Layout node.
func BlockLayout(flavor, label string, options any) *Layout {
	return &Layout{
		Block: &BlockSpec{
			Flavor:  flavor,
			Label:   label,
			Options: options,
		},
	}
}

// ShellLayout returns a com.superlogical.terminal.shell Layout node.
func ShellLayout(label string, opts *TerminalOptions) *Layout {
	var raw any
	if opts != nil {
		raw = opts
	}
	return BlockLayout(FlavorTerminalShell, label, raw)
}

// HorizontalSplit places before on the left and after on the right.
// Ratio is the fraction of space given to before (0 defaults to 0.5).
func HorizontalSplit(ratio float64, before, after *Layout) *Layout {
	return &Layout{
		Split: &SplitSpec{
			Direction: DirectionHorizontal,
			Before:    before,
			After:     after,
			Ratio:     ratio,
		},
	}
}

// VerticalSplit places before above and after below.
// Ratio is the fraction of space given to before (0 defaults to 0.5).
func VerticalSplit(ratio float64, before, after *Layout) *Layout {
	return &Layout{
		Split: &SplitSpec{
			Direction: DirectionVertical,
			Before:    before,
			After:     after,
			Ratio:     ratio,
		},
	}
}

// RevisionResponse is returned by layout mutations that advance the session revision.
type RevisionResponse struct {
	Revision int `json:"revision"`
}

// NewSplitRequest configures session.new_split.
type NewSplitRequest struct {
	SessionID string  `json:"session_id,omitempty"`
	AnchorID  string  `json:"anchor_id,omitempty"`
	WindowID  string  `json:"window_id,omitempty"`
	Direction string  `json:"direction"`
	Side      string  `json:"side"`
	Ratio     float64 `json:"ratio,omitempty"`
	Layout    *Layout `json:"layout"`
	Focus     bool    `json:"focus,omitempty"`
}

// NewSplitResponse is returned by session.new_split.
type NewSplitResponse struct {
	AnchorID string   `json:"anchor_id"`
	BlockIDs []string `json:"block_ids"`
	SplitIDs []string `json:"split_ids"`
	Revision int      `json:"revision"`
}

// NewSplit creates blocks in a new split beside an existing block or split.
func (c *Client) NewSplit(ctx context.Context, req NewSplitRequest) (*NewSplitResponse, error) {
	if req.Layout == nil {
		return nil, fmt.Errorf("rex session.new_split: layout is required")
	}
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out NewSplitResponse
	if err := c.CallSession(ctx, sid, "session.new_split", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NewBlockRequest configures session.new_block (creates a detached block).
type NewBlockRequest struct {
	SessionID string `json:"session_id,omitempty"`
	Flavor    string `json:"flavor"`
	Label     string `json:"label,omitempty"`
	Options   any    `json:"options,omitempty"`
}

// NewBlockResponse is returned by session.new_block.
type NewBlockResponse struct {
	BlockID  string `json:"block_id"`
	Revision int    `json:"revision"`
}

// NewBlock creates a detached block that runs without being placed in a window.
func (c *Client) NewBlock(ctx context.Context, req NewBlockRequest) (*NewBlockResponse, error) {
	if req.Flavor == "" {
		req.Flavor = FlavorTerminalShell
	}
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out NewBlockResponse
	if err := c.CallSession(ctx, sid, "session.new_block", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MoveBlockRequest configures session.move_block.
type MoveBlockRequest struct {
	SessionID     string  `json:"session_id,omitempty"`
	BlockID       string  `json:"block_id"`
	AnchorBlockID string  `json:"anchor_block_id"`
	Direction     string  `json:"direction,omitempty"`
	Side          string  `json:"side,omitempty"`
	Ratio         float64 `json:"ratio,omitempty"`
}

// MoveBlockResponse is returned by session.move_block.
type MoveBlockResponse struct {
	CreatedSplitID string `json:"created_split_id"`
	RemovedSplitID string `json:"removed_split_id,omitempty"`
	Revision       int    `json:"revision"`
}

// MoveBlock moves a block (or places a detached block) beside anchorBlockID.
func (c *Client) MoveBlock(ctx context.Context, req MoveBlockRequest) (*MoveBlockResponse, error) {
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out MoveBlockResponse
	if err := c.CallSession(ctx, sid, "session.move_block", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SwapBlocks exchanges the positions of two placed blocks.
func (c *Client) SwapBlocks(ctx context.Context, session, blockID, otherBlockID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.swap_blocks", map[string]any{
		"block_id":       blockID,
		"other_block_id": otherBlockID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ResizePaneRequest configures session.resize_pane.
type ResizePaneRequest struct {
	SessionID     string  `json:"session_id,omitempty"`
	Direction     string  `json:"direction"`
	BeforeBlockID string  `json:"before_block_id"`
	AfterBlockID  string  `json:"after_block_id"`
	Ratio         float64 `json:"ratio"`
}

// ResizePane moves the split border between before_block_id and after_block_id.
func (c *Client) ResizePane(ctx context.Context, req ResizePaneRequest) (*RevisionResponse, error) {
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out RevisionResponse
	if err := c.CallSession(ctx, sid, "session.resize_pane", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ZoomBlockRequest configures session.zoom_block.
type ZoomBlockRequest struct {
	SessionID string `json:"session_id,omitempty"`
	BlockID   string `json:"block_id,omitempty"`
	WindowID  string `json:"window_id,omitempty"`
	Zoom      *bool  `json:"zoom,omitempty"`
}

// ZoomBlockResponse is returned by session.zoom_block.
type ZoomBlockResponse struct {
	BlockID  string `json:"block_id"`
	Zoomed   bool   `json:"zoomed"`
	Revision int    `json:"revision"`
}

// ZoomBlock maximizes a block, restores the layout, or toggles when Zoom is nil.
func (c *Client) ZoomBlock(ctx context.Context, req ZoomBlockRequest) (*ZoomBlockResponse, error) {
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out ZoomBlockResponse
	if err := c.CallSession(ctx, sid, "session.zoom_block", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FocusBlock focuses blockID and activates its window.
func (c *Client) FocusBlock(ctx context.Context, session, blockID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.focus_block", map[string]any{
		"block_id": c.resolveBlock(blockID),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// FocusDirection moves focus in direction ("left", "right", "up", "down").
func (c *Client) FocusDirection(ctx context.Context, session, direction string, wrap bool) (*RevisionResponse, error) {
	req := map[string]any{"direction": direction}
	if wrap {
		req["wrap"] = true
	}
	var out RevisionResponse
	if err := c.CallSession(ctx, session, "session.focus_direction", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetBlockLabel updates a block's display label.
func (c *Client) SetBlockLabel(ctx context.Context, session, blockID, label string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.set_block_label", map[string]any{
		"block_id": c.resolveBlock(blockID),
		"label":    label,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DetachBlock removes blockID from its window layout while keeping it running.
func (c *Client) DetachBlock(ctx context.Context, session, blockID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.detach_block", map[string]any{
		"block_id": c.resolveBlock(blockID),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseBlockResponse is returned by block.close.
type CloseBlockResponse struct {
	BlockID  string `json:"block_id"`
	Revision int    `json:"revision"`
}

// CloseBlock closes a block and terminates the program running inside it.
func (c *Client) CloseBlock(ctx context.Context, session, blockID string) (*CloseBlockResponse, error) {
	var out CloseBlockResponse
	err := c.CallSession(ctx, session, "block.close", map[string]any{
		"block_id": c.resolveBlock(blockID),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LayerResponse is returned by session.float_block.
type LayerResponse struct {
	LayerID  string `json:"layer_id"`
	Revision int    `json:"revision"`
}

// FloatBlock moves a tiled block into a new floating layer at bounds.
func (c *Client) FloatBlock(ctx context.Context, session, blockID string, bounds Rect) (*LayerResponse, error) {
	var out LayerResponse
	err := c.CallSession(ctx, session, "session.float_block", map[string]any{
		"block_id": c.resolveBlock(blockID),
		"bounds":   bounds,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// NewLayerRequest configures session.new_layer.
type NewLayerRequest struct {
	SessionID string  `json:"session_id,omitempty"`
	WindowID  string  `json:"window_id,omitempty"`
	Bounds    Rect    `json:"bounds"`
	Layout    *Layout `json:"layout"`
	Focus     bool    `json:"focus,omitempty"`
}

// NewLayerResponse is returned by session.new_layer.
type NewLayerResponse struct {
	LayerID  string   `json:"layer_id"`
	BlockIDs []string `json:"block_ids"`
	Revision int      `json:"revision"`
}

// NewLayer creates blocks in a new floating layer at the front of a window.
func (c *Client) NewLayer(ctx context.Context, req NewLayerRequest) (*NewLayerResponse, error) {
	if req.Layout == nil {
		return nil, fmt.Errorf("rex session.new_layer: layout is required")
	}
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out NewLayerResponse
	if err := c.CallSession(ctx, sid, "session.new_layer", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResizeLayer moves or resizes a floating layer.
func (c *Client) ResizeLayer(ctx context.Context, session, layerID string, bounds Rect) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.resize_layer", map[string]any{
		"layer_id": layerID,
		"bounds":   bounds,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetLayerStateRequest configures session.set_layer_state.
type SetLayerStateRequest struct {
	SessionID string `json:"session_id,omitempty"`
	LayerID   string `json:"layer_id"`
	State     string `json:"state"`
	Focus     bool   `json:"focus,omitempty"`
}

// SetLayerState sets a floating layer to "normal" or "minimized".
func (c *Client) SetLayerState(ctx context.Context, req SetLayerStateRequest) (*RevisionResponse, error) {
	sid := c.resolveSession(req.SessionID)
	payload := req
	payload.SessionID = ""
	var out RevisionResponse
	if err := c.CallSession(ctx, sid, "session.set_layer_state", payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RaiseLayer brings a floating layer to the front.
func (c *Client) RaiseLayer(ctx context.Context, session, layerID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.raise_layer", map[string]any{
		"layer_id": layerID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// LowerLayer sends a floating layer to the back (or below another layer if belowLayerID is set).
func (c *Client) LowerLayer(ctx context.Context, session, layerID, belowLayerID string) (*RevisionResponse, error) {
	req := map[string]any{"layer_id": layerID}
	if belowLayerID != "" {
		req["below_layer_id"] = belowLayerID
	}
	var out RevisionResponse
	if err := c.CallSession(ctx, session, "session.lower_layer", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseLayer closes a floating layer and every block inside it.
func (c *Client) CloseLayer(ctx context.Context, session, layerID string) (*RevisionResponse, error) {
	var out RevisionResponse
	err := c.CallSession(ctx, session, "session.close_layer", map[string]any{
		"layer_id": layerID,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}
