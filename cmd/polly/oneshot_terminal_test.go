package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// The subprocess exercises main's real flag parsing, provider loop, signal
// handling and output routing, without rebuilding a second application binary.
func TestOneShotCLIProcess(t *testing.T) {
	encoded := os.Getenv("POLLY_TEST_ONESHOT_ARGS")
	if encoded == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"polly"}, args...)
	main()
	os.Exit(0)
}

func TestOneShotTerminalContract(t *testing.T) {
	skipIfWindows(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY contract fixture requires python3")
	}
	command := exec.Command(python, "testdata/oneshot_terminal.py", os.Args[0], t.TempDir())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("one-shot terminal contract: %v\n%s", err, output)
	}
	t.Log(string(output))
}
