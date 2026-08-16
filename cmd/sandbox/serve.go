package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
)

// runServe implements the `serve` subcommand: it builds a warm Pool and
// serves RunRequests over a unix socket and/or a loopback HTTP listener
// until interrupted, then shuts down gracefully.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	sf := registerSandboxFlags(fs)

	minH := fs.Int("min", 1, "minimum warm holders kept ready")
	maxH := fs.Int("max", 4, "maximum holders under load")
	idle := fs.Duration("idle", 5*time.Minute, "close holders idle longer than this (0 = never)")
	unixPath := fs.String("unix", "", "unix socket path to listen on (e.g. /run/user/1000/sandbox.sock)")
	httpAddr := fs.String("http", "", "HTTP listen address (loopback only unless -http-allow-remote), e.g. 127.0.0.1:8099")
	allowRemote := fs.Bool("http-allow-remote", false, "permit a non-loopback -http bind (DANGEROUS: no auth)")
	_ = fs.Parse(args)

	if *unixPath == "" && *httpAddr == "" {
		fmt.Fprintln(os.Stderr, "serve: at least one of -unix or -http is required")
		return 2
	}

	// serve supplies commands per-request, so no Cmd and no stdin here
	o, err := sf.buildOpts(nil, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := sandbox.NewPool(ctx, o, sandbox.PoolConfig{
		Min:         *minH,
		Max:         *maxH,
		IdleTimeout: *idle,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "serve: creating pool:", err)
		return 1
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pool.Close(closeCtx)
	}()

	srv := sandbox.NewServer(pool)

	// listener errors flow here; first one triggers shutdown
	errCh := make(chan error, 2)

	// --- unix listener ---
	var unixListener net.Listener
	if *unixPath != "" {
		// remove a stale socket from a previous run
		if _, statErr := os.Stat(*unixPath); statErr == nil {
			_ = os.Remove(*unixPath)
		}
		var lc net.ListenConfig
		l, lerr := lc.Listen(ctx, "unix", *unixPath)
		if lerr != nil {
			fmt.Fprintln(os.Stderr, "serve: listen unix:", lerr)
			return 1
		}
		// owner-only access to the socket
		if cherrr := os.Chmod(*unixPath, 0o600); cherrr != nil {
			fmt.Fprintln(os.Stderr, "serve: chmod socket:", cherrr)
			_ = l.Close()
			return 1
		}
		unixListener = l
		fmt.Fprintf(os.Stderr, "serve: listening on unix://%s\n", *unixPath)
		go func() { errCh <- srv.ServeUnix(l) }()
	}

	// --- http listener ---
	var httpServer *http.Server
	if *httpAddr != "" {
		if !*allowRemote && !isLoopbackAddr(*httpAddr) {
			fmt.Fprintf(os.Stderr, "serve: -http %q is not loopback; refusing (pass -http-allow-remote to override, but note there is no authentication)\n", *httpAddr)
			return 2
		}
		httpServer = &http.Server{
			Addr:              *httpAddr,
			Handler:           srv,
			ReadHeaderTimeout: 10 * time.Second,
		}
		fmt.Fprintf(os.Stderr, "serve: listening on http://%s (no authentication; keep it loopback)\n", *httpAddr)
		go func() {
			err := httpServer.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}

	// wait for a signal or a fatal listener error
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nserve: shutting down...")
	case err := <-errCh:
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve: listener error:", err)
		}
	}

	// graceful shutdown of each listener
	shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if httpServer != nil {
		_ = httpServer.Shutdown(shutCtx)
	}
	if unixListener != nil {
		_ = unixListener.Close()
		_ = os.Remove(*unixPath)
	}
	// pool.Close runs via the deferred func above
	return 0
}

// isLoopbackAddr reports whether addr's host is a loopback address (or the
// empty/unspecified host, which we treat as non-loopback to be safe).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// no port? treat the whole thing as host
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false // e.g. ":8099" binds all interfaces — not loopback
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
