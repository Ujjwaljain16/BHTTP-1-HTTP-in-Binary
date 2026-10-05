package main

import (
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bhttp/internal/server"
)

// The two client check tools that other people point at their own clients are
// also pointed at ours, so they cannot quietly drift out of step with it.

func findPython(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs external tools")
	}
	for _, name := range []string{"python3", "python"} {
		if exec.Command(name, "-c", "import sys").Run() == nil {
			return name
		}
	}
	t.Skip("no working Python interpreter")
	return ""
}

func runTool(t *testing.T, python string, timeout time.Duration, args ...string) string {
	t.Helper()
	cmd := exec.Command(python, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
		t.Fatalf("%s did not finish within %v:\n%s", args[0], timeout, out)
	}
	if err != nil || !strings.Contains(string(out), " 0 failed") {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func TestClientSurvivesAServerThatMisbehaves(t *testing.T) {
	python := findPython(t)
	tool := filepath.Join("..", "..", "tests", "interop", "client_attack.py")
	out := runTool(t, python, 5*time.Minute, tool, "--", binPath)
	if !strings.Contains(out, "PASS  a custom response header with an upper-case name is accepted") {
		t.Errorf("the scenario that found the response-header bug did not run:\n%s", out)
	}
}

func TestClientPutsExactlyTheDocumentedBytesOnTheWire(t *testing.T) {
	python := findPython(t)
	srv, err := server.New(server.Config{Root: filepath.Join("..", "..", "conformance", "www")})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()

	tool := filepath.Join("..", "..", "tests", "interop", "client_wire.py")
	runTool(t, python, 5*time.Minute, tool, "--server", l.Addr().String(), "--", binPath)
}
