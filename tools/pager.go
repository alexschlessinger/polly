package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// The bounded-paging limits shared by every tool that pages text into model
// context (read_artifact, read_file). Responses stay under PageMaxBytes so a
// page is never itself externalized to an artifact.
const (
	PageDefaultLines = 200
	PageMaxLines     = 500
	PageMaxBytes     = 40 << 10
	// PageScanMaxLine bounds how much of one physical line is held in memory;
	// longer lines page as byte_offset placeholders.
	PageScanMaxLine = 1 << 20
)

// ErrPageSizeMismatch reports that the content's size did not match the size
// the caller declared, e.g. because it changed between stat and read. Callers
// wrap it with their own context.
var ErrPageSizeMismatch = errors.New("content size does not match its declared size")

// PageLines renders numbered lines from r and cannot fail on content shape:
// physical lines longer than PageScanMaxLine become bounded placeholders that
// point at their byte_offset instead of aborting the scan, and mid-line
// response-cap truncation reports the exact continuation byte_offset. noun
// names the content kind in messages (e.g. "artifact", "file").
func PageLines(ctx context.Context, r io.Reader, noun string, offset, limit int, query string) (string, error) {
	// Reserved room guarantees a truncation note always fits under the cap.
	const noteReserve = 64
	br := bufio.NewReaderSize(r, 64<<10)
	var out bytes.Buffer
	lineNumber := 0
	emitted := 0
	var lineStart int64
	truncated := false
	continueAt := int64(-1)
	for {
		line, err := readPhysicalLine(ctx, br, PageScanMaxLine, query)
		if err != nil {
			return "", fmt.Errorf("scan %s: %w", noun, err)
		}
		if !line.readAny {
			break
		}
		lineNumber++
		start := lineStart
		lineStart += line.rawLen
		if line.sawNewline {
			lineStart++
		}
		if lineNumber < offset || (query != "" && !line.matched) {
			if !line.sawNewline {
				break
			}
			continue
		}
		overlong := int64(len(line.held)) < line.rawLen
		var entry string
		partialAllowed := false
		switch {
		case overlong && query != "":
			entry = fmt.Sprintf("%d: [query matches inside %d-byte line; read with byte_offset=%d]\n", lineNumber, line.rawLen, start)
		case overlong:
			entry = fmt.Sprintf("%d: [line is %d bytes; exceeds inline limit; read with byte_offset=%d]\n", lineNumber, line.rawLen, start)
		default:
			display := line.held
			if len(display) > 0 && display[len(display)-1] == '\r' {
				display = display[:len(display)-1]
			}
			entry = fmt.Sprintf("%d: %s\n", lineNumber, display)
			partialAllowed = true
		}
		if out.Len()+len(entry) > PageMaxBytes-noteReserve {
			prefix := len(fmt.Sprintf("%d: ", lineNumber))
			continueAt = start
			if room := PageMaxBytes - noteReserve - out.Len(); partialAllowed && room > prefix {
				// Back a cut that splits a rune up to its boundary so the
				// character renders on the continuation page instead of as a
				// replacement on both; binary content keeps the raw cut so
				// paging still advances.
				cut := pageUTF8Boundary(entry, room)
				if room-cut >= utf8.UTFMax {
					cut = room
				}
				if cut > prefix {
					out.WriteString(entry[:cut])
					continueAt = start + int64(cut-prefix)
				}
			}
			truncated = true
			break
		}
		out.WriteString(entry)
		emitted++
		if emitted >= limit {
			if line.sawNewline {
				if _, peekErr := br.Peek(1); peekErr == nil {
					truncated = true
				} else if peekErr != io.EOF {
					return "", fmt.Errorf("scan %s: %w", noun, peekErr)
				}
			}
			break
		}
		if !line.sawNewline {
			break
		}
	}
	if out.Len() == 0 {
		if query != "" {
			return CapPageText(fmt.Sprintf("No matches for %q at or after line %d.", query, offset)), nil
		}
		return fmt.Sprintf("%s has no content at or after line %d.", capitalizeNoun(noun), offset), nil
	}
	if truncated {
		if continueAt >= 0 {
			fmt.Fprintf(&out, "\n[output truncated; continue with byte_offset=%d]", continueAt)
		} else {
			fmt.Fprintf(&out, "\n[bounded %s output truncated]", noun)
		}
	}
	return CapPageText(out.String()), nil
}

type physicalLine struct {
	held       []byte
	rawLen     int64
	sawNewline bool
	matched    bool
	readAny    bool
}

// readPhysicalLine reads one newline-terminated physical line, holding at most
// keep bytes and stream-discarding the rest while counting exact byte lengths.
// The query is matched against the complete line via a carry search, so a hit
// past the held window or spanning chunk boundaries is still found.
func readPhysicalLine(ctx context.Context, br *bufio.Reader, keep int, query string) (physicalLine, error) {
	var line physicalLine
	var carry []byte
	for {
		if err := ctx.Err(); err != nil {
			return physicalLine{}, err
		}
		chunk, readErr := br.ReadSlice('\n')
		if len(chunk) > 0 {
			line.readAny = true
			segment := chunk
			if segment[len(segment)-1] == '\n' {
				line.sawNewline = true
				segment = segment[:len(segment)-1]
			}
			line.rawLen += int64(len(segment))
			if len(line.held) < keep {
				take := min(keep-len(line.held), len(segment))
				line.held = append(line.held, segment[:take]...)
			}
			if query != "" && !line.matched && len(segment) > 0 {
				probe := segment
				if len(carry) > 0 {
					probe = append(append([]byte(nil), carry...), segment...)
				}
				line.matched = bytes.Contains(probe, []byte(query))
				if overlap := len(query) - 1; !line.matched && overlap > 0 {
					if len(probe) > overlap {
						probe = probe[len(probe)-overlap:]
					}
					carry = append(carry[:0], probe...)
				}
			}
		}
		if line.sawNewline || readErr == io.EOF {
			return line, nil
		}
		if readErr != nil && readErr != bufio.ErrBufferFull {
			return physicalLine{}, readErr
		}
	}
}

// PageByteWindow returns a raw byte window of r; paging with the reported next
// byte_offset recovers any content exactly, regardless of line structure. noun
// and name identify the content in the window header (e.g. "artifact", its ID),
// and size is the caller's authoritative content length: a shorter or longer
// stream returns ErrPageSizeMismatch.
func PageByteWindow(ctx context.Context, r io.Reader, noun, name string, size, byteOffset int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := io.CopyN(io.Discard, r, byteOffset); err != nil {
		if err == io.EOF {
			return "", ErrPageSizeMismatch
		}
		return "", err
	}
	window := int64(PageMaxBytes - 256)
	remaining := size - byteOffset
	readLimit := min(window, remaining)
	data, err := io.ReadAll(io.LimitReader(r, readLimit+1))
	if err != nil {
		return "", err
	}
	hasSentinel := int64(len(data)) > readLimit
	if int64(len(data)) < readLimit || (remaining > readLimit) != hasSentinel {
		return "", ErrPageSizeMismatch
	}
	data = data[:readLimit]
	end := byteOffset + int64(len(data))
	if end < size && len(data) > 0 {
		// Trim a rune split by the window edge so text pages cleanly; binary
		// content is left whole so paging always advances.
		if r, size := utf8.DecodeLastRune(data); r == utf8.RuneError && size == 1 {
			cut := len(data)
			for cut > 0 && len(data)-cut < utf8.UTFMax && (data[cut-1]&0xc0) == 0x80 {
				cut--
			}
			if cut > 0 && len(data)-cut < utf8.UTFMax && (data[cut-1]&0xc0) == 0xc0 {
				cut--
			}
			if len(data)-cut < utf8.UTFMax {
				data = data[:cut]
				end = byteOffset + int64(len(data))
			}
		}
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, "[%s %s; bytes %d-%d of %d; raw window]\n", noun, name, byteOffset, end, size)
	out.Write(data)
	if end < size {
		fmt.Fprintf(&out, "\n[%s continues; next byte_offset=%d]", noun, end)
	}
	return CapPageText(out.String()), nil
}

// CapPageText bounds a page response to PageMaxBytes on a rune boundary and
// keeps it valid UTF-8 (content may contain arbitrary bytes) without
// increasing its bounded byte size.
func CapPageText(text string) string {
	if len(text) > PageMaxBytes {
		text = text[:pageUTF8Boundary(text, PageMaxBytes)]
	}
	return string(bytes.ToValidUTF8([]byte(text), []byte("?")))
}

func pageUTF8Boundary(s string, end int) int {
	if end >= len(s) {
		return len(s)
	}
	for end > 0 && (s[end]&0xc0) == 0x80 {
		end--
	}
	return end
}

func capitalizeNoun(noun string) string {
	if noun == "" {
		return noun
	}
	return strings.ToUpper(noun[:1]) + noun[1:]
}
