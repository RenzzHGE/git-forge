// Entry point for the Go forge server.
//
// The Node entry (main.ts, byte-identical) spawns this binary through the
// app.ts shim: the binary binds an ephemeral port, prints `PORT <n>` on its
// only stdout line, and serves. Everything else is logged to stderr.
//
// Shutdown: SIGTERM/SIGINT, or stdin EOF (the parent closes the pipe when
// it exits), both shut the server down and exit 0.

package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge-go] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("PORT %d\n", port)

	server := &http.Server{Handler: http.HandlerFunc(dispatchHandler)}

	// Shutdown triggers: a signal, or stdin EOF (parent gone).
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-sig:
		case <-done:
		}
		_ = server.Close()
	}()
	go func() {
		// The parent holds our stdin open; EOF means the parent is gone.
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = server.Close()
	}()

	err = server.Serve(ln)
	close(done)
	if err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "[forge-go] serve: %v\n", err)
		os.Exit(1)
	}
}
