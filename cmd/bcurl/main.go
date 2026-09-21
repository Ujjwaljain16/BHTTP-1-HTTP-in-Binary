// Command bcurl fetches one path from a server speaking the binary protocol
// and writes the body to standard output.
//
//	bcurl [-v] <host>:<port>[/path]
//
// Exit status: 0 on success, 1 if the server answered 4xx or 5xx, 2 for bad
// usage, 3 if the exchange itself failed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"bhttp/internal/client"
	"bhttp/internal/wire"
)

const dialTimeout = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bcurl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, "usage: bcurl [-v] <host>:<port>[/path]") }
	verbose := fs.Bool("v", false, "hexdump every frame that crosses the connection (to stderr)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	addr, path, err := parseTarget(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 2
	}

	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 3
	}
	if *verbose {
		tap := wire.NewTap(stderr)
		defer tap.Flush()
		conn = tap.Wrap(conn)
	}

	c := client.New(conn)
	defer c.Close()

	res, err := c.Get(path, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "bcurl: %v\n", err)
		return 3
	}
	if res.Status >= 400 {
		fmt.Fprintf(stderr, "bcurl: server answered %d\n", res.Status)
		return 1
	}
	return 0
}

// parseTarget splits "host:port/path" into a dial address and a request path.
// The port is mandatory because the protocol has no well-known one.
func parseTarget(target string) (addr, path string, err error) {
	authority, path := target, "/"
	if i := strings.IndexByte(target, '/'); i >= 0 {
		authority, path = target[:i], target[i:]
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil || host == "" || port == "" {
		return "", "", errors.New("target must look like host:port/path")
	}
	return net.JoinHostPort(host, port), path, nil
}
