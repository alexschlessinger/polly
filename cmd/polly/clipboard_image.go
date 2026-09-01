package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/images"
	_ "golang.org/x/image/tiff"
)

// Terminals expose only text through bracketed paste, so grabbing an image
// from the system clipboard means asking the platform. Every backend lands a
// PNG file in the attachment cache; the composer then treats it exactly like a
// dropped image path.

const clipboardCaptureTimeout = 5 * time.Second

var errNoClipboardImage = errors.New("no image on the clipboard")

// clipboardImageCommand is one candidate invocation. Commands either stream
// image bytes to stdout or write destPath themselves; either way the result is
// verified (and re-encoded to PNG when needed) before anyone sees the file.
type clipboardImageCommand struct {
	argv     []string
	toStdout bool
}

// clipboardImageCommands returns the platform's candidates in preference
// order. Pure function of its inputs so tests can cover each platform without
// executing anything.
func clipboardImageCommands(goos string, getenv func(string) string, lookPath func(string) (string, error), destPath string) []clipboardImageCommand {
	available := func(name string) bool {
		_, err := lookPath(name)
		return err == nil
	}
	var commands []clipboardImageCommand
	switch goos {
	case "darwin":
		if available("pngpaste") {
			commands = append(commands, clipboardImageCommand{argv: []string{"pngpaste", destPath}})
		}
		// osascript ships with macOS. Screenshots put PNG on the clipboard;
		// images copied from browsers and Preview are often TIFF, which the
		// verifier converts to PNG after the fact.
		for _, class := range []string{"PNGf", "TIFF"} {
			commands = append(commands, clipboardImageCommand{
				argv: []string{"osascript",
					"-e", fmt.Sprintf("set imgData to the clipboard as «class %s»", class),
					"-e", fmt.Sprintf("set f to open for access POSIX file \"%s\" with write permission", appleScriptEscape(destPath)),
					"-e", "try",
					"-e", "set eof of f to 0",
					"-e", "write imgData to f",
					"-e", "close access f",
					"-e", "on error errMessage number errNumber",
					"-e", "close access f",
					"-e", "error errMessage number errNumber",
					"-e", "end try",
				},
			})
		}
	case "windows":
		commands = append(commands, clipboardImageCommand{
			argv: []string{"powershell", "-NoProfile", "-STA", "-Command",
				"Add-Type -AssemblyName System.Windows.Forms; " +
					"$img = [System.Windows.Forms.Clipboard]::GetImage(); " +
					"if ($img -eq $null) { exit 1 }; " +
					"$img.Save('" + strings.ReplaceAll(destPath, "'", "''") + "', [System.Drawing.Imaging.ImageFormat]::Png)"},
		})
	default:
		if getenv("WAYLAND_DISPLAY") != "" && available("wl-paste") {
			commands = append(commands, clipboardImageCommand{argv: []string{"wl-paste", "--type", "image/png"}, toStdout: true})
		}
		if available("xclip") {
			commands = append(commands, clipboardImageCommand{argv: []string{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"}, toStdout: true})
		}
	}
	return commands
}

func appleScriptEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

// captureClipboardImage tries the platform's clipboard readers and returns the
// path of a verified PNG in dir. The distinction between "no image is on the
// clipboard" and "this system has no reader" is kept: the first is routine,
// the second is actionable.
func captureClipboardImage(ctx context.Context, dir string) (string, error) {
	destPath := filepath.Join(dir, fmt.Sprintf("clip-%d.png", time.Now().UnixNano()))
	commands := clipboardImageCommands(runtime.GOOS, os.Getenv, exec.LookPath, destPath)
	if len(commands) == 0 {
		if runtime.GOOS == "linux" {
			return "", errors.New("no clipboard image reader found (install wl-clipboard or xclip)")
		}
		return "", errors.New("clipboard images are not supported on this platform")
	}

	for _, command := range commands {
		cmdCtx, cancel := context.WithTimeout(ctx, clipboardCaptureTimeout)
		cmd := exec.CommandContext(cmdCtx, command.argv[0], command.argv[1:]...)
		var stdout bytes.Buffer
		if command.toStdout {
			cmd.Stdout = &stdout
		}
		err := cmd.Run()
		cancel()
		if err != nil {
			_ = os.Remove(destPath)
			continue
		}
		if command.toStdout {
			if stdout.Len() == 0 {
				continue
			}
			if err := os.WriteFile(destPath, stdout.Bytes(), 0o600); err != nil {
				return "", err
			}
		}
		if err := normalizeCapturedImage(destPath); err != nil {
			_ = os.Remove(destPath)
			continue
		}
		return destPath, nil
	}
	return "", errNoClipboardImage
}

// normalizeCapturedImage verifies the captured file decodes as a raster image
// and rewrites non-PNG payloads (macOS TIFF clipboards) as PNG so the rest of
// the pipeline sees one format.
func normalizeCapturedImage(path string) error {
	data, err := images.ReadBoundedFile(path, maxLocalImageBytes)
	if err != nil {
		return err
	}
	_, format, err := images.Validate(data)
	if err != nil {
		return fmt.Errorf("captured %w", err)
	}
	if format == "png" {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
