package docker

import (
	"bufio"
	"encoding/binary"
	"io"
)

// Exec streams without a TTY are multiplexed: an 8-byte header names the
// stream (1 stdout, 2 stderr) and the payload length, big-endian, in its
// last four bytes.
const (
	streamStdout byte = 1
	streamStderr byte = 2
)

// stdcopyReader yields the stdout payloads of a multiplexed stream and
// forwards stderr payloads elsewhere.
type stdcopyReader struct {
	reader    *bufio.Reader
	stderr    io.Writer
	remaining int
	header    [8]byte
}

func newStdcopyReader(reader *bufio.Reader, stderr io.Writer) *stdcopyReader {
	if stderr == nil {
		stderr = io.Discard
	}
	return &stdcopyReader{reader: reader, stderr: stderr}
}

func (d *stdcopyReader) Read(p []byte) (int, error) {
	for d.remaining == 0 {
		if _, err := io.ReadFull(d.reader, d.header[:]); err != nil {
			return 0, err
		}
		size := int(binary.BigEndian.Uint32(d.header[4:]))
		switch d.header[0] {
		case streamStdout:
			d.remaining = size
		case streamStderr:
			if _, err := io.CopyN(d.stderr, d.reader, int64(size)); err != nil {
				return 0, err
			}
		default:
			if _, err := io.CopyN(io.Discard, d.reader, int64(size)); err != nil {
				return 0, err
			}
		}
	}
	if len(p) > d.remaining {
		p = p[:d.remaining]
	}
	n, err := d.reader.Read(p)
	d.remaining -= n
	return n, err
}

// stdcopyWriter frames writes as one stream of a multiplexed stream. The
// daemon does this for a real exec; the test fake uses it.
type stdcopyWriter struct {
	writer io.Writer
	stream byte
}

func (w stdcopyWriter) Write(p []byte) (int, error) {
	var header [8]byte
	header[0] = w.stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := w.writer.Write(header[:]); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}
