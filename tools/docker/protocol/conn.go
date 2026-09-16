package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrFrameTooLarge reports a line longer than MaxFrameBytes.
var ErrFrameTooLarge = errors.New("protocol frame exceeds the size limit")

// Conn frames JSON lines over a reader and a writer. Reads are sequential;
// writes may come from several goroutines.
type Conn struct {
	reader *bufio.Reader
	writer io.Writer
	mu     sync.Mutex
}

// NewConn wraps a reader and writer.
func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{reader: bufio.NewReaderSize(r, 64<<10), writer: w}
}

// Read returns the next frame. It returns io.EOF when the stream ends
// between frames and ErrFrameTooLarge for an oversize line.
func (c *Conn) Read() (Frame, error) {
	var line []byte
	for {
		chunk, err := c.reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxFrameBytes {
			return Frame{}, ErrFrameTooLarge
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(bytes.TrimSpace(line)) == 0 {
			return Frame{}, io.EOF
		}
		if errors.Is(err, io.EOF) {
			return Frame{}, io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return c.Read()
	}
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("decode frame: %w", err)
	}
	return frame, nil
}

// Write sends one frame.
func (c *Conn) Write(frame Frame) error {
	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if len(encoded) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	encoded = append(encoded, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.writer.Write(encoded)
	return err
}

// Send marshals body into a frame of the given type and ID.
func (c *Conn) Send(id uint64, typ string, body any) error {
	frame := Frame{ID: id, Type: typ}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s body: %w", typ, err)
		}
		frame.Body = encoded
	}
	return c.Write(frame)
}

// Decode unmarshals a frame body.
func Decode[T any](frame Frame) (T, error) {
	var body T
	if len(frame.Body) == 0 {
		return body, nil
	}
	if err := json.Unmarshal(frame.Body, &body); err != nil {
		return body, fmt.Errorf("decode %s body: %w", frame.Type, err)
	}
	return body, nil
}
