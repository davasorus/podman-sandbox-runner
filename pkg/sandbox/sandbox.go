// Package sandbox runs untrusted commands in ephemeral, locked-down containers.
package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Opts configures a sandboxed run.
type Opts struct {
	Image     string
	Cmd       []string
	Timeout   time.Duration
	MemMB     int64
	CPUs      float64
	User      string   // uid:gid; empty = image default
	Binds     []string // host:container[:ro] — ro is enforced regardless
	Env       []string // KEY=VAL
	WithStdin bool
	Backend   string // "auto" | "docker" (docker API: docker & podman) | "k8s" (phase 2)
}

// Result is the outcome of a sandboxed run.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	OOMKilled  bool   `json:"oom_killed"`
	TimedOut   bool   `json:"timed_out"`
	Error      string `json:"error,omitempty"`
}

// Backend executes a run. stdout/stderr receive live output; pass buffers to capture.
// stdin may be nil when Opts.WithStdin is false.
type Backend interface {
	Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error)
}

// New returns the backend for o.Backend.
func New(o Opts) (Backend, error) {
	switch o.Backend {
	case "", "auto", "docker":
		return &dockerBackend{}, nil
	case "k8s":
		return nil, fmt.Errorf("k8s backend not implemented yet")
	default:
		return nil, fmt.Errorf("unknown backend %q", o.Backend)
	}
}

// Run is the convenience entrypoint: picks a backend and executes.
func Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	b, err := New(o)
	if err != nil {
		return Result{}, err
	}
	return b.Run(ctx, o, stdin, stdout, stderr)
}
