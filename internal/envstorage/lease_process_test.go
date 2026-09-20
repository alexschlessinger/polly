//go:build unix

package envstorage

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStorageLeaseProcessHelper(t *testing.T) {
	path := os.Getenv("POLLY_TEST_ENV_LEASE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	lease, err := OpenLease(path)
	if err != nil {
		fmt.Println("busy")
		return
	}
	defer lease.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestStorageLeaseAcrossProcesses(t *testing.T) {
	r, _ := fixture(t)
	path := filepath.Join(r.Control, "lease")
	lease, err := OpenLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	helper := func() *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStorageLeaseProcessHelper$")
		cmd.Env = append(os.Environ(), "POLLY_TEST_ENV_LEASE="+path)
		return cmd
	}
	cmd := helper()
	in, send, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()
	defer in.Close()
	cmd.Stdin = in
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("child lease: %q %v", line, err)
	}
	if err := lease.Exclusive(func() error { t.Error("cleanup with another process using environment"); return nil }); err == nil {
		t.Fatal("missing cross-process exclusion")
	}
	send.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Exclusive(func() error {
		data, err := helper().CombinedOutput()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(data), "busy\n") {
			return fmt.Errorf("new session entered cleanup: %s", data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
