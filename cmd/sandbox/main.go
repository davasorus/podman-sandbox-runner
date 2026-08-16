package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
)

// version is overwritten at release build time via
// -ldflags "-X main.version={{.Version}}" (see .goreleaser.yaml).
var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("sandbox", version)
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "bench" {
		os.Exit(runBench(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "serve" {
		os.Exit(runServe(os.Args[2:]))
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: sandbox run [flags] -- <command...> | sandbox serve [flags] | sandbox version")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	sf := registerSandboxFlags(fs)
	stdinFlag := fs.Bool("i", false, "pipe stdin into the container")
	jsonFlag := fs.Bool("json", false, "emit result as JSON instead of streaming")
	via := fs.String("via", "", "submit to a running daemon instead of executing locally: unix:///path or http://host:port")
	_ = fs.Parse(os.Args[2:])

	cmd := fs.Args()
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "no command given after flags")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- daemon-client path ---
	if *via != "" {
		os.Exit(runViaDaemon(ctx, *via, cmd, *stdinFlag, *jsonFlag))
	}

	// --- local execution path ---
	o, err := sf.buildOpts(cmd, *stdinFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	var stdin io.Reader
	if o.WithStdin {
		stdin = os.Stdin
	}

	if *jsonFlag {
		var outBuf, errBuf bytes.Buffer
		res, err := sandbox.Run(ctx, o, stdin, &outBuf, &errBuf)
		res.Stdout, res.Stderr = outBuf.String(), errBuf.String()
		if err != nil {
			res.Error = err.Error()
		}
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: encoding result:", err)
		}
		if err != nil {
			os.Exit(125)
		}
		os.Exit(0)
	}

	res, err := sandbox.Run(ctx, o, stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(125)
	}
	os.Exit(res.ExitCode)
}

// runViaDaemon submits the command to a daemon at endpoint and renders the
// response the same way local run does. Returns the process exit code.
func runViaDaemon(ctx context.Context, endpoint string, cmd []string, withStdin, asJSON bool) int {
	req := sandbox.RunRequest{Cmd: cmd}
	if withStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: reading stdin:", err)
			return 125
		}
		req.Stdin = data
	}

	resp, err := sandbox.Submit(ctx, endpoint, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		return 125
	}

	if asJSON {
		out := sandbox.Result{
			ExitCode:  resp.ExitCode,
			Stdout:    string(resp.Stdout),
			Stderr:    string(resp.Stderr),
			TimedOut:  resp.TimedOut,
			OOMKilled: resp.OOMKilled,
			Error:     resp.Error,
		}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox: encoding result:", err)
		}
		if resp.Error != "" {
			return 125
		}
		return 0
	}

	_, _ = os.Stdout.Write(resp.Stdout)
	_, _ = os.Stderr.Write(resp.Stderr)
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "sandbox:", resp.Error)
		return 125
	}
	return resp.ExitCode
}
