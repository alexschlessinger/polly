package main

import (
	"strings"
	"testing"
)

func TestReadAllInputAcceptsLongLinesAndNormalizesEndings(t *testing.T) {
	long := strings.Repeat("x", 200*1024)
	got, err := readAllInput(strings.NewReader("first\r\n" + long + "\r\nlast\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "first\n"+long+"\nlast" {
		t.Fatalf("readAllInput mangled the input (len %d)", len(got))
	}
	if got, _ := readAllInput(strings.NewReader("")); got != "" {
		t.Fatalf("empty input = %q", got)
	}
}
