package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/schema"
)

// viewImageTool lets the model attach an image it discovered itself — a
// screenshot a command just produced, a file in the workspace, or an image
// URL — to the conversation. The returned media enters the artifact store and
// the next model request through the standard tool-media path. File and
// network access honor the registry's base sandbox policy so the tool cannot
// see what a sandboxed command could not.
type viewImageTool struct {
	NativeTool
	registry *ToolRegistry
}

// NewViewImageTool creates the view_image tool bound to registry's sandbox
// policy.
func NewViewImageTool(registry *ToolRegistry) OutputTool {
	return &viewImageTool{registry: registry}
}

func (t *viewImageTool) GetName() string { return "view_image" }

func (t *viewImageTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(
		"view_image",
		"Attach an image to the conversation so you can see it. Use this to inspect a screenshot, figure, or other raster image file (PNG, JPEG, WebP, GIF, BMP) by filesystem path or http(s) URL.",
		schema.Params{
			"source": schema.S("Filesystem path or http(s) URL of the image to view"),
		},
		"source",
	)
}

func (t *viewImageTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	output, err := t.ExecuteOutput(ctx, args)
	return output.Text, err
}

func (t *viewImageTool) ExecuteOutput(ctx context.Context, raw map[string]any) (ToolOutput, error) {
	source := strings.TrimSpace(Args(raw).String("source"))
	if source == "" {
		return ToolOutput{}, fmt.Errorf("source is required")
	}
	sandboxCfg, sandboxActive, err := t.registry.SandboxReadPolicy()
	if err != nil {
		return ToolOutput{}, fmt.Errorf("resolve sandbox policy: %w", err)
	}

	var data []byte
	var name string
	if u, parseErr := url.Parse(source); parseErr == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if sandboxActive && !sandboxCfg.AllowNetwork {
			return ToolOutput{}, fmt.Errorf("the sandbox policy denies network access; view_image can only fetch URLs when the sandbox allows network")
		}
		data, name, err = fetchImageURL(ctx, source, u)
	} else {
		data, name, err = readImageFile(t.registry, source)
	}
	if err != nil {
		return ToolOutput{}, err
	}

	norm, err := images.NormalizeForModel(data, name)
	if err != nil {
		return ToolOutput{}, err
	}
	return ToolOutput{
		Text:  fmt.Sprintf("Attached image %s (%s, %dx%d, %d bytes).", norm.FileName, norm.MIMEType, norm.Width, norm.Height, len(norm.Data)),
		Media: []ToolMedia{{Data: norm.Data, MIMEType: norm.MIMEType, Name: norm.FileName}},
	}, nil
}

func readImageFile(registry *ToolRegistry, path string) ([]byte, string, error) {
	abs, err := resolveLocalPath(path)
	if err != nil {
		return nil, "", err
	}
	routes, resolved := localRoutes(abs)
	if err := checkReadPolicy(registry, routes...); err != nil {
		return nil, "", err
	}
	f, _, err := openLocalRegular(resolved, os.O_RDONLY, 0)
	if err != nil {
		return nil, "", describeOpenError("read", abs, err)
	}
	defer f.Close()
	data, err := images.ReadBoundedFrom(f, images.MaxSourceBytes)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", abs, err)
	}
	return data, filepath.Base(abs), nil
}

func fetchImageURL(ctx context.Context, source string, u *url.URL) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", source, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch %s: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: HTTP %d %s", source, resp.StatusCode, resp.Status)
	}
	if resp.ContentLength > images.MaxSourceBytes {
		return nil, "", fmt.Errorf("fetch %s: response exceeds the %d MiB download limit", source, images.MaxSourceBytes>>20)
	}
	// Bound even chunked or dishonest responses before buffering them. The
	// extra byte distinguishes an exactly-full response from an oversized one.
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(images.MaxSourceBytes)+1))
	if err != nil {
		return nil, "", fmt.Errorf("read response from %s: %w", source, err)
	}
	if len(data) > images.MaxSourceBytes {
		return nil, "", fmt.Errorf("fetch %s: response exceeds the %d MiB download limit", source, images.MaxSourceBytes>>20)
	}
	name := filepath.Base(u.Path)
	if name == "" || name == "/" || name == "." {
		name = "downloaded-image"
	}
	return data, name, nil
}
