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
)

var binPath string

// The tests run the real executable so argument handling, exit codes and
// startup output are checked as a user would meet them.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bserve-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "bserve.exe")
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
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), stderr.String()
		}
		return 0, stderr.String()
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("bserve %v did not exit; stderr: %s", args, stderr.String())
		return -1, ""
	}
}

func makeRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "www")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("served by the real binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBadArguments(t *testing.T) {
	root := makeRoot(t)
	tests := map[string][]string{
		"no arguments":      {},
		"only a root":       {root},
		"too many":          {root, "9000", "extra"},
		"port not a number": {root, "http"},
		"port too large":    {root, "70000"},
		"negative port":     {root, "-1"},
		"empty port":        {root, ""},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			code, stderr := exitCode(t, args...)
			if code != 2 || stderr == "" {
				t.Errorf("exit %d, stderr %q; want 2 with an explanation", code, stderr)
			}
		})
	}
}

func TestUnusableRoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "nope"),
		"a file":            file,
	} {
		t.Run(name, func(t *testing.T) {
			code, stderr := exitCode(t, root, "0")
			if code != 1 || !strings.Contains(stderr, "bserve:") {
				t.Errorf("exit %d, stderr %q", code, stderr)
			}
		})
	}
}

type running struct {
	cmd  *exec.Cmd
	port string
}

func start(t *testing.T, root, port string) *running {
	t.Helper()
	cmd := exec.Command(binPath, root, port)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- s
		io.Copy(io.Discard, out)
	}()
	select {
	case s := <-line:
		addr, _, ok := strings.Cut(strings.TrimPrefix(s, "listening on "), ",")
		i := strings.LastIndex(addr, ":")
		if !strings.HasPrefix(s, "listening on ") || !ok || i < 0 {
			t.Fatalf("unexpected startup line %q", s)
		}
		return &running{cmd: cmd, port: addr[i+1:]}
	case <-time.After(10 * time.Second):
		t.Fatal("server never announced that it was listening")
		return nil
	}
}

func TestServesRequestsOnceStarted(t *testing.T) {
	srv := start(t, makeRoot(t), "0")

	conn, err := net.Dial("tcp", "127.0.0.1:"+srv.port)
	if err != nil {
		t.Fatal(err)
	}
	c := client.New(conn)
	c.Timeout = 5 * time.Second
	defer c.Close()

	for i := 0; i < 3; i++ {
		var body bytes.Buffer
		res, err := c.Get("/", &body)
		if err != nil || res.Status != 200 || body.String() != "served by the real binary" {
			t.Fatalf("request %d: %+v %q %v", i, res, body.String(), err)
		}
	}
	if res, err := c.Get("/missing", io.Discard); err != nil || res.Status != 404 {
		t.Fatalf("missing file: %+v %v", res, err)
	}
}

func TestPortAlreadyInUse(t *testing.T) {
	first := start(t, makeRoot(t), "0")
	code, stderr := exitCode(t, makeRoot(t), first.port)
	if code != 1 || !strings.Contains(stderr, "bserve:") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
}
