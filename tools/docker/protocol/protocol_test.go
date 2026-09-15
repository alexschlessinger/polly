package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFramesRoundTripOnePerLine(t *testing.T) {
	var buf bytes.Buffer
	conn := NewConn(strings.NewReader(""), &buf)
	if err := conn.Send(7, TypeExecute, Execute{Tool: "bash", Args: map[string]any{"command": "printf 'a\nb'"}}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(8, TypeHeartbeat, nil); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(buf.String(), "\n"); lines != 2 {
		t.Fatalf("frames span %d lines:\n%s", lines, buf.String())
	}
	reader := NewConn(&buf, io.Discard)
	frame, err := reader.Read()
	if err != nil || frame.ID != 7 || frame.Type != TypeExecute {
		t.Fatalf("frame %+v, %v", frame, err)
	}
	req, err := Decode[Execute](frame)
	if err != nil || req.Tool != "bash" || req.Args["command"] != "printf 'a\nb'" {
		t.Fatalf("execute body %+v, %v", req, err)
	}
	if frame, err := reader.Read(); err != nil || frame.ID != 8 || len(frame.Body) != 0 {
		t.Fatalf("heartbeat frame %+v, %v", frame, err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v", err)
	}
}

func TestOversizeAndTruncatedFrames(t *testing.T) {
	huge := `{"id":1,"type":"result","body":{"text":"` + strings.Repeat("x", MaxFrameBytes) + `"}}` + "\n"
	if _, err := NewConn(strings.NewReader(huge), io.Discard).Read(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize frame = %v", err)
	}
	if _, err := NewConn(strings.NewReader(`{"id":1,"type":"pong"}`), io.Discard).Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame = %v", err)
	}
	if _, err := NewConn(strings.NewReader("not json\n"), io.Discard).Read(); err == nil {
		t.Fatal("malformed frame accepted")
	}
}
