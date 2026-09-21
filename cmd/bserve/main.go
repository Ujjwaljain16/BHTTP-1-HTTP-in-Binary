// Command bserve serves a directory over the binary protocol.
//
//	bserve <root> <port>
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"bhttp/internal/server"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: bserve <root> <port>")
		return 2
	}
	port, err := strconv.Atoi(args[1])
	if err != nil || port < 0 || port > 65535 {
		fmt.Fprintf(os.Stderr, "bserve: %q is not a valid port (use 0-65535; 0 picks a free one)\n", args[1])
		return 2
	}

	srv, err := server.New(server.Config{
		Root:   args[0],
		Logger: log.New(os.Stderr, "", log.LstdFlags),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "bserve: %v\n", err)
		return 1
	}

	lis, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bserve: %v\n", err)
		return 1
	}
	fmt.Printf("listening on %s, serving %s\n", lis.Addr(), args[0])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	if err := srv.Serve(lis); err != nil {
		fmt.Fprintf(os.Stderr, "bserve: %v\n", err)
		return 1
	}
	return 0
}
