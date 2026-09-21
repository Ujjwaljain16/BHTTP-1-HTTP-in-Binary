package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bhttp/internal/client"
	"bhttp/internal/server"
)

var binPath string

// The tests run the real executable so flags and startup output are checked as
// a user meets them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bchaos-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "bchaos.exe")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func exitCode(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stderr.String()
}

func TestBadUsage(t *testing.T) {
	tests := map[string][]string{
		"no arguments":        {},
		"target missing":      {"-chunk", "1"},
		"target without port": {"-target", "localhost"},
		"negative chunk":      {"-target", "localhost:1", "-chunk", "-1"},
		"negative delay":      {"-target", "localhost:1", "-delay", "-1s"},
		"unknown flag":        {"-target", "localhost:1", "-frobnicate"},
		"stray argument":      {"-target", "localhost:1", "extra"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			code, stderr := exitCode(t, args...)
			if code != 2 || !(strings.Contains(stderr, "usage:") || strings.Contains(stderr, "bchaos:")) {
				t.Errorf("exit %d, stderr %q; want 2 with an explanation", code, stderr)
			}
		})
	}
}

func TestListenAddressInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	code, stderr := exitCode(t, "-target", "127.0.0.1:1", "-listen", l.Addr().String())
	if code != 1 || !strings.Contains(stderr, "bchaos:") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}

func TestRelaysRequestsWithTheRequestedDisturbance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "www")
	srv, err := server.New(server.Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()

	cmd := exec.Command(binPath, "-target", l.Addr().String(), "-listen", "127.0.0.1:0", "-chunk", "3", "-noise")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
		io.Copy(io.Discard, out)
	}()
	var announced string
	select {
	case announced = <-line:
	case <-time.After(10 * time.Second):
		t.Fatal("bchaos never announced its address")
	}
	addr, _, _ := strings.Cut(strings.TrimPrefix(announced, "listening on "), ",")
	for _, want := range []string{"chunk=3", "noise=true"} {
		if !strings.Contains(announced, want) {
			t.Errorf("startup line %q does not mention %s", announced, want)
		}
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(conn)
	c.Timeout = 10 * time.Second
	defer c.Close()

	want, err := os.ReadFile(filepath.Join(root, "edge-16385.bin"))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	res, err := c.Get("/edge-16385.bin", &body)
	if err != nil || res.Status != 200 || !bytes.Equal(body.Bytes(), want) {
		t.Fatalf("through bchaos: %+v, %d body bytes, %v", res, body.Len(), err)
	}
}
