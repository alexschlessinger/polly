package rex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CaptureFormat selects the encoding returned by com.superlogical.terminal.format.
const (
	CapturePlain = "plain"
	CaptureHTML  = "html"
	CaptureVT    = "vt"
)

// CaptureOptions configures com.superlogical.terminal.format.
type CaptureOptions struct {
	Format     string `json:"format,omitempty"`
	Trim       bool   `json:"trim,omitempty"`
	Unwrap     bool   `json:"unwrap,omitempty"`
	MaxBytes   int    `json:"max_bytes,omitempty"`
	SinglePage bool   `json:"-"`

	ExtraCharsets        bool `json:"extra_charsets,omitempty"`
	ExtraCursor          bool `json:"extra_cursor,omitempty"`
	ExtraHyperlink       bool `json:"extra_hyperlink,omitempty"`
	ExtraKeyboard        bool `json:"extra_keyboard,omitempty"`
	ExtraKittyKeyboard   bool `json:"extra_kitty_keyboard,omitempty"`
	ExtraModes           bool `json:"extra_modes,omitempty"`
	ExtraPalette         bool `json:"extra_palette,omitempty"`
	ExtraProtection      bool `json:"extra_protection,omitempty"`
	ExtraPWD             bool `json:"extra_pwd,omitempty"`
	ExtraScrollingRegion bool `json:"extra_scrolling_region,omitempty"`
	ExtraStyle           bool `json:"extra_style,omitempty"`
	ExtraTabstops        bool `json:"extra_tabstops,omitempty"`
}

// CaptureResult holds the captured screen and scrollback content.
type CaptureResult struct {
	Content string `json:"content"`
	EOF     bool   `json:"eof"`
	ID      int64  `json:"id,omitempty"`
	Total   int    `json:"total,omitempty"`
}

// BlobChunk is one page returned by com.superlogical.terminal.blob_read.
type BlobChunk struct {
	Data  string `json:"data"`
	EOF   bool   `json:"eof"`
	ID    int64  `json:"id"`
	Total int    `json:"total"`
}

// Capture reads a terminal block's screen and scrollback via
// com.superlogical.terminal.format. When the server splits a large response
// into a blob (!EOF && ID != 0) and opts.SinglePage is false, Capture
// automatically follows com.superlogical.terminal.blob_read until EOF.
func (c *Client) Capture(ctx context.Context, session, blockID string, opts CaptureOptions) (*CaptureResult, error) {
	var first CaptureResult
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "format", opts, &first); err != nil {
		return nil, err
	}
	if first.EOF || first.ID == 0 || opts.SinglePage {
		return &first, nil
	}

	var buf strings.Builder
	buf.WriteString(first.Content)
	offset := len(first.Content)
	for {
		chunk, err := c.ReadBlob(ctx, session, blockID, first.ID, offset, opts.MaxBytes)
		if err != nil {
			return nil, fmt.Errorf("rex capture blob %d at offset %d: %w", first.ID, offset, err)
		}
		buf.WriteString(chunk.Data)
		offset += len(chunk.Data)
		if chunk.EOF || len(chunk.Data) == 0 {
			first.Content = buf.String()
			first.EOF = chunk.EOF
			if chunk.Total > 0 {
				first.Total = chunk.Total
			}
			return &first, nil
		}
	}
}

// ReadBlob reads a byte range from a server-cached terminal result blob.
func (c *Client) ReadBlob(ctx context.Context, session, blockID string, blobID int64, offset, maxBytes int) (*BlobChunk, error) {
	args := map[string]any{
		"id":     blobID,
		"offset": offset,
	}
	if maxBytes > 0 {
		args["max_bytes"] = maxBytes
	}
	var out BlobChunk
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "blob_read", args, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Write queues literal text into a terminal block via com.superlogical.terminal.write.
func (c *Client) Write(ctx context.Context, session, blockID, data string) (int, error) {
	var out struct {
		BytesWritten int `json:"bytes_written"`
	}
	err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "write", map[string]any{
		"data": data,
	}, &out)
	if err != nil {
		return 0, err
	}
	return out.BytesWritten, nil
}

// SendKey sends one or more named keys (for example "enter", "ctrl+c", "shift+g")
// to a terminal block via "rex send-key".
func (c *Client) SendKey(ctx context.Context, session, blockID string, keys ...string) error {
	if len(keys) == 0 {
		return fmt.Errorf("rex send-key: at least one key is required")
	}
	bid := c.resolveBlock(blockID)
	if bid == "" {
		return fmt.Errorf("rex send-key: block_id is required")
	}
	subArgs := []string{"send-key"}
	if sid := c.resolveSession(session); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	subArgs = append(subArgs, "-b", bid)
	subArgs = append(subArgs, keys...)
	_, err := c.runRaw(ctx, subArgs, nil, "send-key")
	return err
}

// ProcessDescriptor describes one OS process associated with a terminal block.
type ProcessDescriptor struct {
	PID         int    `json:"pid,omitempty"`
	Name        string `json:"name,omitempty"`
	Argv0       string `json:"argv0,omitempty"`
	Path        string `json:"path,omitempty"`
	InvokedPath string `json:"invoked_path,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	User        string `json:"user,omitempty"`
	UserID      int    `json:"user_id,omitempty"`
}

// ExitStatus reports how a terminal's program last exited.
type ExitStatus struct {
	ExitCode int    `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ProcessInfo is returned by com.superlogical.terminal.process.
type ProcessInfo struct {
	Child      *ProcessDescriptor  `json:"child,omitempty"`
	Foreground *ProcessDescriptor  `json:"foreground,omitempty"`
	Ancestors  []ProcessDescriptor `json:"ancestors,omitempty"`
	Home       string              `json:"home,omitempty"`
	LastExit   *ExitStatus         `json:"last_exit,omitempty"`
}

// Process inspects the processes running in a terminal block and its last exit status.
func (c *Client) Process(ctx context.Context, session, blockID string) (*ProcessInfo, error) {
	var out ProcessInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "process", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TerminalMode describes one DEC/ANSI terminal mode reported by terminal.modes.
type TerminalMode struct {
	Name    string `json:"name"`
	Number  int    `json:"number"`
	Set     bool   `json:"set"`
	ANSI    bool   `json:"ansi,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// ModesInfo is returned by com.superlogical.terminal.modes.
type ModesInfo struct {
	Screen string         `json:"screen"`
	Modes  []TerminalMode `json:"modes"`
}

// Modes queries the active screen ("primary" or "alternate") and terminal modes.
func (c *Client) Modes(ctx context.Context, session, blockID string) (*ModesInfo, error) {
	var out ModesInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "modes", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListeningPort describes a TCP port held open by a process in a terminal.
type ListeningPort struct {
	Address  string `json:"address,omitempty"`
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Process  string `json:"process,omitempty"`
}

// PortsInfo is returned by com.superlogical.terminal.ports.
type PortsInfo struct {
	Status    string          `json:"status,omitempty"`
	Ports     []ListeningPort `json:"ports,omitempty"`
	Processes json.RawMessage `json:"processes,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	ID        int64           `json:"id,omitempty"`
}

// Ports lists TCP ports listened on by processes attached to a terminal block.
func (c *Client) Ports(ctx context.Context, session, blockID string) (*PortsInfo, error) {
	var out PortsInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "ports", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ProgramStatusRecord is one OSC 7501 status record stored on a terminal.
type ProgramStatusRecord struct {
	ID       string `json:"id,omitempty"`
	State    string `json:"state"`
	Kind     string `json:"kind,omitempty"`
	App      string `json:"app,omitempty"`
	Title    string `json:"title,omitempty"`
	Msg      string `json:"msg,omitempty"`
	Progress *int   `json:"progress,omitempty"`
}

// ProgramStatusInfo is returned by com.superlogical.terminal.program_status.
type ProgramStatusInfo struct {
	Records []ProgramStatusRecord `json:"records"`
}

// ProgramStatus returns all active OSC 7501 program status records on a terminal.
func (c *Client) ProgramStatus(ctx context.Context, session, blockID string) (*ProgramStatusInfo, error) {
	var out ProgramStatusInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "program_status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PWDInfo is returned by com.superlogical.terminal.pwd.
type PWDInfo struct {
	PWD   string             `json:"pwd,omitempty"`
	Owner *ProcessDescriptor `json:"owner,omitempty"`
}

// PWD returns the working directory last reported by a program in the terminal.
func (c *Client) PWD(ctx context.Context, session, blockID string) (*PWDInfo, error) {
	var out PWDInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "pwd", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TitleInfo is returned by com.superlogical.terminal.title.
type TitleInfo struct {
	Title string             `json:"title,omitempty"`
	Owner *ProcessDescriptor `json:"owner,omitempty"`
}

// Title returns the terminal's current title and the process that set it.
func (c *Client) Title(ctx context.Context, session, blockID string) (*TitleInfo, error) {
	var out TitleInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "title", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DesiredSize records the size requested by one connected client.
type DesiredSize struct {
	ClientID  string `json:"client_id"`
	Columns   int    `json:"columns"`
	Rows      int    `json:"rows"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// SizeInfo is returned by com.superlogical.terminal.size.
type SizeInfo struct {
	Columns      int           `json:"columns"`
	Rows         int           `json:"rows"`
	CellWidthPX  int           `json:"cell_width_px,omitempty"`
	CellHeightPX int           `json:"cell_height_px,omitempty"`
	Owner        string        `json:"owner,omitempty"`
	Desired      []DesiredSize `json:"desired,omitempty"`
}

// Size returns a terminal's dimensions, owner, and client-requested sizes.
func (c *Client) Size(ctx context.Context, session, blockID string) (*SizeInfo, error) {
	var out SizeInfo
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "size", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Resize requests a new terminal size in columns and rows.
func (c *Client) Resize(ctx context.Context, session, blockID string, size TerminalSize) error {
	return c.CallBlock(ctx, session, blockID, CreatorTerminal, "resize", size, nil)
}

// Clear clears the terminal's screen and scrollback (no-op in alternate screen).
func (c *Client) Clear(ctx context.Context, session, blockID string) error {
	return c.CallBlock(ctx, session, blockID, CreatorTerminal, "clear", nil, nil)
}

// Reset resets the terminal emulator state and clears screen and scrollback.
func (c *Client) Reset(ctx context.Context, session, blockID string) error {
	return c.CallBlock(ctx, session, blockID, CreatorTerminal, "reset", nil, nil)
}

// ClipboardRead reads text recently copied to the clipboard by a terminal program.
func (c *Client) ClipboardRead(ctx context.Context, session, blockID string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.CallBlock(ctx, session, blockID, CreatorTerminal, "clipboard_read", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
