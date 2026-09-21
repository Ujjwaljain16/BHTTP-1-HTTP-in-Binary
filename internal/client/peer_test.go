package client

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAgainstExternalServer exercises the client against a server written by
// someone else. It only runs when BHTTP_PEER names one (host:port) and
// BHTTP_PEER_ROOT points at the directory that server is serving, so the same
// test works for any independent implementation.
func TestAgainstExternalServer(t *testing.T) {
	addr, root := os.Getenv("BHTTP_PEER"), os.Getenv("BHTTP_PEER_ROOT")
	if addr == "" || root == "" {
		t.Skip("set BHTTP_PEER and BHTTP_PEER_ROOT to run against an external server")
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := New(conn)
	c.Timeout = 10 * time.Second
	defer c.Close()

	index, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(filepath.Join(root, "binary.bin"))
	if err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		path   string
		status int
		body   []byte
	}{
		{"/", 200, index},
		{"/index.html", 200, index},
		{"/binary.bin", 200, binary},
		{"/missing-file", 404, nil},
		{"/a:b", 400, nil},
		{"/../secret", 400, nil},
		{"/binary.bin", 200, binary},
		{"/index.html", 200, index},
	}
	for i, s := range steps {
		var body bytes.Buffer
		res, err := c.Get(s.path, &body)
		if err != nil {
			t.Fatalf("request %d (%s) on the shared connection: %v", i+1, s.path, err)
		}
		if res.Status != s.status || !bytes.Equal(body.Bytes(), s.body) {
			t.Errorf("request %d (%s): status %d with %d body bytes, want %d with %d", i+1, s.path, res.Status, body.Len(), s.status, len(s.body))
		}
	}
}
