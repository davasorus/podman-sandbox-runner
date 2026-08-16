package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// runMCP implements the `mcp` subcommand: it exposes the sandbox as MCP
// tools over stdio, for an MCP client (e.g. a coding agent) to call.
//
// Two execution modes, chosen by -pool:
//   - one-shot (default): each tool call runs a fresh sandbox.Run — cold
//     container per call, fully isolated, per-call image override allowed.
//   - pooled (-pool): the server holds a warm sandbox.Pool and each call
//     execs into a holder — much lower per-call latency for repeated calls,
//     at the documented same-trust-domain cost of pool mode. The image is
//     fixed at pool creation, so per-call image overrides are rejected.
//
// Tools:
//   - run_sandbox: run an explicit command (cmd array) with optional stdin.
//   - run_script:  run a script string via an interpreter that reads stdin
//     (e.g. python3, sh, node), i.e. [interpreter, "-"] with the script fed
//     on stdin — no writable mount needed, identical in both modes.
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	sf := registerSandboxFlags(fs)

	usePool := fs.Bool("pool", false, "hold a warm pool (reuse holders across calls) instead of one-shot per call")
	minH := fs.Int("min", 1, "pooled mode: minimum warm holders kept ready")
	maxH := fs.Int("max", 4, "pooled mode: maximum holders under load")
	idle := fs.Duration("idle", 5*time.Minute, "pooled mode: close holders idle longer than this (0 = never)")
	_ = fs.Parse(args)

	// mcp supplies commands per tool call, so no Cmd and no ambient stdin
	baseOpts, err := sf.buildOpts(nil, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp:", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// In pooled mode, stand up the warm pool once for the server lifetime.
	var pool *sandbox.Pool
	if *usePool {
		p, perr := sandbox.NewPool(ctx, baseOpts, sandbox.PoolConfig{
			Min:         *minH,
			Max:         *maxH,
			IdleTimeout: *idle,
		})
		if perr != nil {
			fmt.Fprintln(os.Stderr, "mcp: creating pool:", perr)
			return 1
		}
		pool = p
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = pool.Close(closeCtx)
		}()
	}

	server := buildMCPServer(baseOpts, pool)

	// Run blocks until the client disconnects or ctx is cancelled; per the
	// SDK, a non-nil return is the normal end of a single-session stdio
	// server (client closed the pipe / "server is closing"), not a failure.
	// So we exit 0 on any clean end and only surface a genuinely unexpected
	// error. A cancelled context (we were signaled) is silent.
	err = server.Run(ctx, &mcp.StdioTransport{})
	if err == nil || ctx.Err() != nil || errors.Is(err, io.EOF) {
		return 0
	}
	if strings.Contains(err.Error(), "server is closing") ||
		strings.Contains(err.Error(), "connection closed") {
		return 0 // normal client disconnect
	}
	fmt.Fprintln(os.Stderr, "mcp:", err)
	return 1
}

// buildMCPServer constructs the MCP server with both sandbox tools
// registered. Extracted so tests can exercise the exact registration path
// runMCP uses. The input structs' JSON schemas are inferred by the SDK from
// their struct tags. pool may be nil (one-shot mode).
func buildMCPServer(base sandbox.Opts, pool *sandbox.Pool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "podman-sandbox-runner",
		Version: version,
	}, nil)

	exec := &sandboxExecutor{base: base, pool: pool}

	mcp.AddTool(server, &mcp.Tool{
		Name: "run_sandbox",
		Description: "Run a command in a locked-down, ephemeral sandbox container " +
			"(no network, read-only rootfs, dropped capabilities, non-root, memory/CPU/time limits). " +
			"Returns stdout, stderr, exit code, and whether the run timed out or was OOM-killed.",
	}, exec.runSandbox)

	mcp.AddTool(server, &mcp.Tool{
		Name: "run_script",
		Description: "Run a script in a locked-down, ephemeral sandbox container by feeding it to an " +
			"interpreter on stdin (e.g. python3, sh, node). Same isolation as run_sandbox. " +
			"Returns stdout, stderr, exit code, and whether the run timed out or was OOM-killed.",
	}, exec.runScript)

	return server
}

// sandboxExecutor carries the base opts and (in pooled mode) the shared
// pool, and provides the two tool handlers.
type sandboxExecutor struct {
	base sandbox.Opts
	pool *sandbox.Pool // nil in one-shot mode
}

// runSandboxInput is the input shape for run_sandbox.
type runSandboxInput struct {
	Cmd   []string `json:"cmd" jsonschema:"the command and its arguments to run, e.g. [\"echo\",\"hello\"]"`
	Stdin string   `json:"stdin,omitempty" jsonschema:"optional data piped to the command's standard input"`
	Image string   `json:"image,omitempty" jsonschema:"optional container image override for this call (one-shot mode only)"`
}

// runScriptInput is the input shape for run_script.
type runScriptInput struct {
	Script      string `json:"script" jsonschema:"the script source to execute"`
	Interpreter string `json:"interpreter" jsonschema:"an interpreter that reads a script from stdin via '-', e.g. python3, sh, bash, node"`
	Image       string `json:"image,omitempty" jsonschema:"optional container image override for this call (one-shot mode only)"`
}

// runOutput is the structured result returned to the caller (in addition to
// a human-readable text summary).
type runOutput struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	OOMKilled bool   `json:"oom_killed"`
}

func (e *sandboxExecutor) runSandbox(ctx context.Context, _ *mcp.CallToolRequest, in runSandboxInput) (*mcp.CallToolResult, runOutput, error) {
	if len(in.Cmd) == 0 {
		return nil, runOutput{}, fmt.Errorf("cmd must not be empty")
	}
	return e.execute(ctx, in.Cmd, in.Stdin, in.Image)
}

func (e *sandboxExecutor) runScript(ctx context.Context, _ *mcp.CallToolRequest, in runScriptInput) (*mcp.CallToolResult, runOutput, error) {
	if strings.TrimSpace(in.Script) == "" {
		return nil, runOutput{}, fmt.Errorf("script must not be empty")
	}
	if strings.TrimSpace(in.Interpreter) == "" {
		return nil, runOutput{}, fmt.Errorf("interpreter must not be empty")
	}
	// Deliver the script on stdin and run `<interpreter> -`, which reads the
	// program from stdin. Mode-agnostic and needs no writable mount.
	cmd := []string{in.Interpreter, "-"}
	return e.execute(ctx, cmd, in.Script, in.Image)
}

// execute dispatches to the pool (pooled mode) or a one-shot Run, applying
// a per-call image override where the mode allows it. stdin is passed as a
// reader only when non-empty (a nil io.Reader means "no stdin").
func (e *sandboxExecutor) execute(ctx context.Context, cmd []string, stdin, image string) (*mcp.CallToolResult, runOutput, error) {
	var outBuf, errBuf bytes.Buffer
	var res sandbox.Result
	var err error

	var stdinReader io.Reader
	if stdin != "" {
		stdinReader = strings.NewReader(stdin)
	}

	if e.pool != nil {
		if image != "" && image != e.base.Image {
			return nil, runOutput{}, fmt.Errorf("image override is not supported in pooled mode (image is fixed at pool creation: %s); restart with -image, or use one-shot mode", e.base.Image)
		}
		res, err = e.pool.Run(ctx, cmd, stdinReader, &outBuf, &errBuf)
	} else {
		o := e.base
		o.Cmd = cmd
		o.WithStdin = stdinReader != nil
		if image != "" {
			o.Image = image
		}
		res, err = sandbox.Run(ctx, o, stdinReader, &outBuf, &errBuf)
	}

	if err != nil {
		return nil, runOutput{}, err
	}

	out := runOutput{
		ExitCode:  res.ExitCode,
		Stdout:    outBuf.String(),
		Stderr:    errBuf.String(),
		TimedOut:  res.TimedOut,
		OOMKilled: res.OOMKilled,
	}

	summary := fmt.Sprintf("exit=%d timed_out=%v oom_killed=%v\n--- stdout ---\n%s\n--- stderr ---\n%s",
		out.ExitCode, out.TimedOut, out.OOMKilled, out.Stdout, out.Stderr)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: summary}},
	}, out, nil
}
