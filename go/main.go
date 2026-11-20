// Entry point for the Go forge server.
//
// The Node entry (main.ts, byte-identical) spawns this binary through the
// app.ts shim: the binary binds an ephemeral port, prints `PORT <n>` on its
// only stdout line, and serves. Everything else is logged to stderr.
//
// Transport: raw TCP + epoll (epoll.go), not net/http — one epoll fd, one
// accept loop and a bounded worker pool, with the same handlers behind it.
//
// Shutdown: SIGTERM/SIGINT, or stdin EOF (the parent closes the pipe when
// it exits), both shut the server down and exit 0.

package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
)

func main() {
	// The store is small and the trial is short-lived: collect half as
	// often as the default (GOGC 100 -> 200). GC is pure overhead here —
	// nothing in the heap needs prompt reclamation — and on a 2-CPU
	// sandbox every GC cycle is time the bench client cannot use.
	// (From opt-prof; kept with the epoll transport — the old
	// net.Listen/net/http path it sat next to is gone.)
	debug.SetGCPercent(200)
	srv, err := newEpollServer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge-go] listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PORT %d\n", srv.port)

	// Shutdown triggers: a signal, or stdin EOF (parent gone).
	done := make(chan struct{})
	var once sync.Once
	trigger := func() { once.Do(func() { close(done) }) }
	go func() {
		sig := make(chan os.Signal, 2)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		trigger()
	}()
	go func() {
		// The parent holds our stdin open; EOF means the parent is gone.
		_, _ = io.Copy(io.Discard, os.Stdin)
		trigger()
	}()

	<-done
	srv.close()
}
