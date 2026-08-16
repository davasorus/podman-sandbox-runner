package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Pool keeps a warm holder alive and executes runs inside it via the
// backend's exec mechanism, trading inter-run isolation for latency.
//
// SECURITY TRADEOFF: unlike one-shot Run, consecutive runs in a pool
// share a container/pod. Between runs the pool reaps workload
// processes and clears /work and /tmp, but this hygiene is
// best-effort: runs in one pool must belong to the same trust domain.
// For mutually untrusted callers, use one-shot Run or separate pools.
//
// Hygiene differs by backend. The docker backend reaps atomically
// (kill broadcast as the workload uid, which cannot touch the
// root-side holder). The k8s backend has no per-exec user, so holder
// and workload share a uid and reaping is enumeration-based — a
// hostile fork-storm could theoretically race it; the pids limit
// bounds that race. See the README for the full comparison.
//
// Runs are serialized: Pool.Run holds an internal mutex. For
// concurrency, create multiple pools.
//
// Opts.Cmd is ignored (each Run supplies its own command);
// Opts.Timeout applies per run. On k8s, Opts.NetWait is applied once
// at pool creation.
type Pool struct {
	mu     sync.Mutex
	impl   poolImpl
	opts   Opts
	broken error
	closed bool
}

// poolImpl is the per-backend pool machinery. All methods are called
// under Pool.mu.
type poolImpl interface {
	// run executes cmd in the holder; it owns per-run timeout handling
	// and returns TimedOut in the Result when applicable.
	run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error)
	// cleanup reaps workload processes and clears scratch. Best-effort.
	cleanup()
	// checkHolder returns an error if the holder is no longer usable.
	checkHolder() error
	// close removes the holder and releases resources. Idempotent.
	close(ctx context.Context) error
}

// NewPool creates and starts the holder for the backend selected by
// o.Backend. The caller must Close the pool to remove it.
func NewPool(ctx context.Context, o Opts) (*Pool, error) {
	if o.User == "" {
		return nil, fmt.Errorf("pool: Opts.User must be set")
	}
	var impl poolImpl
	var err error
	switch o.Backend {
	case "", "auto", "docker":
		impl, err = newDockerPool(ctx, o)
	case "k8s":
		impl, err = newK8sPool(ctx, o)
	default:
		return nil, fmt.Errorf("pool: unknown backend %q", o.Backend)
	}
	if err != nil {
		return nil, err
	}
	return &Pool{impl: impl, opts: o}, nil
}

// Run executes cmd in the warm holder. Runs are serialized. stdin may
// be nil.
func (p *Pool) Run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var res Result
	if p.closed {
		return res, fmt.Errorf("pool: closed")
	}
	if p.broken != nil {
		return res, fmt.Errorf("pool: unusable: %w", p.broken)
	}

	res, runErr := p.impl.run(ctx, cmd, stdin, stdout, stderr)

	// between-runs hygiene, then fail closed if the holder died
	p.impl.cleanup()
	if err := p.impl.checkHolder(); err != nil {
		p.broken = err
		if runErr == nil {
			runErr = fmt.Errorf("pool: %w (create a new pool)", err)
		}
	}
	return res, runErr
}

// Close removes the holder. Idempotent.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.impl.close(ctx)
}
