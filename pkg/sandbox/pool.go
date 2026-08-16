package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// holder wraps one backend pool instance. A holder runs at most one
// workload at a time; its own mutex guards run+cleanup+checkHolder as an
// indivisible unit so a concurrent dispatch can't observe it mid-hygiene.
type holder struct {
	mu   sync.Mutex
	impl poolImpl
	dead error
}

// Pool dispatches runs across one or more warm holders. With size 1 it
// behaves exactly as a single-holder pool; with size N it serves up to N
// runs concurrently, additional runs blocking until a holder frees.
//
// SECURITY TRADEOFF: see poolImpl / the backend docs. Concurrency does not
// change the between-run hygiene guarantee (each holder reaps and clears
// scratch between its own runs); it only adds throughput. Runs dispatched
// to different holders are as isolated from each other as separate pools.
type Pool struct {
	opts    Opts
	idle    chan *holder // buffered, cap == size; holds currently-idle holders
	all     []*holder    // every holder, for Close
	mu      sync.Mutex
	closed  bool
	brokenN int // count of holders that have died
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

// NewPool creates and starts `size` warm holders for the backend selected
// by o.Backend. size < 1 is treated as 1. The caller must Close the pool.
func NewPool(ctx context.Context, o Opts, size int) (*Pool, error) {
	if o.User == "" {
		return nil, fmt.Errorf("pool: Opts.User must be set")
	}
	if size < 1 {
		size = 1
	}

	newImpl := func() (poolImpl, error) {
		switch o.Backend {
		case "", "auto", "docker":
			return newDockerPool(ctx, o)
		case "k8s":
			return newK8sPool(ctx, o)
		default:
			return nil, fmt.Errorf("pool: unknown backend %q", o.Backend)
		}
	}

	p := &Pool{
		opts: o,
		idle: make(chan *holder, size),
		all:  make([]*holder, 0, size),
	}

	for i := 0; i < size; i++ {
		impl, err := newImpl()
		if err != nil {
			// tear down any holders already created before failing
			_ = p.Close(context.Background())
			return nil, fmt.Errorf("pool: creating holder %d/%d: %w", i+1, size, err)
		}
		h := &holder{impl: impl}
		p.all = append(p.all, h)
		p.idle <- h
	}
	return p, nil
}

// Run acquires an idle holder (blocking until one is free or ctx is done),
// executes cmd, runs between-run hygiene, and returns the holder to the
// idle set. If the holder dies during the run it is NOT returned; when all
// holders have died the pool is unusable.
func (p *Pool) Run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return res, fmt.Errorf("pool: closed")
	}
	if p.brokenN >= len(p.all) {
		p.mu.Unlock()
		return res, fmt.Errorf("pool: unusable: all holders have died (create a new pool)")
	}
	p.mu.Unlock()

	// acquire an idle holder or give up if the caller's context ends
	var h *holder
	select {
	case h = <-p.idle:
	case <-ctx.Done():
		return res, fmt.Errorf("pool: acquiring holder: %w", ctx.Err())
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.dead != nil {
		// shouldn't happen (dead holders aren't returned to idle), but
		// guard anyway rather than run against a corpse
		return res, fmt.Errorf("pool: acquired holder is dead: %w", h.dead)
	}

	res, runErr := h.impl.run(ctx, cmd, stdin, stdout, stderr)

	// between-runs hygiene, then liveness check
	h.impl.cleanup()
	if err := h.impl.checkHolder(); err != nil {
		h.dead = err
		p.mu.Lock()
		p.brokenN++
		p.mu.Unlock()
		if runErr == nil {
			runErr = fmt.Errorf("pool: holder died: %w", err)
		}
		// do NOT return h to idle; it stays out of rotation
		return res, runErr
	}

	// healthy: back into rotation
	p.idle <- h
	return res, runErr
}

// Close tears down every holder. Idempotent.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	holders := p.all
	p.mu.Unlock()

	var firstErr error
	for _, h := range holders {
		if err := h.impl.close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
