package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// holder wraps one backend pool instance. A holder runs at most one
// workload at a time; its own mutex guards run+cleanup+checkHolder as an
// indivisible unit so a concurrent dispatch can't observe it mid-hygiene.
type holder struct {
	mu       sync.Mutex
	impl     poolImpl
	dead     error
	lastUsed time.Time
}

// poolImpl is the per-backend pool machinery. All methods are called with
// the owning holder's mutex held.
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

// PoolConfig configures a Pool's autoscaling behavior.
type PoolConfig struct {
	// Min holders kept warm at all times (>= 0). Min == 0 permits the
	// pool to scale to zero when idle, at the cost of a cold create on
	// the next run.
	Min int
	// Max holders under load (>= 1 and >= Min). Runs beyond Max block
	// until a holder frees.
	Max int
	// IdleTimeout closes holders idle longer than this, down to Min.
	// Zero disables idle reaping (holders live until Close).
	IdleTimeout time.Duration
}

func (c PoolConfig) normalize() (PoolConfig, error) {
	if c.Min < 0 {
		return c, fmt.Errorf("pool: Min must be >= 0")
	}
	if c.Max < 1 {
		c.Max = 1
	}
	if c.Max < c.Min {
		return c, fmt.Errorf("pool: Max (%d) must be >= Min (%d)", c.Max, c.Min)
	}
	return c, nil
}

// Pool dispatches runs across an autoscaling set of warm holders. It keeps
// at least cfg.Min holders warm, grows on demand up to cfg.Max, and closes
// idle holders (down to Min) after cfg.IdleTimeout.
//
// SECURITY TRADEOFF: see poolImpl / backend docs. Autoscaling does not
// change the between-run hygiene guarantee (each holder reaps and clears
// scratch between its own runs). Holders created to meet demand are fresh,
// known-good instances identical to those NewPool creates; a holder that
// dies mid-run is never reused — it is removed and, if demand requires,
// replaced by a new one.
type Pool struct {
	opts Opts
	cfg  PoolConfig

	mu     sync.Mutex
	all    map[*holder]struct{} // every live (registered) holder
	idle   chan *holder         // idle holders available for dispatch (cap Max)
	closed bool

	newImpl func(context.Context) (poolImpl, error)

	reaperStop chan struct{}
	reaperDone chan struct{}
}

// NewPool creates a Pool, eagerly starting cfg.Min warm holders. The
// caller must Close the pool to release resources.
func NewPool(ctx context.Context, o Opts, cfg PoolConfig) (*Pool, error) {
	if o.User == "" {
		return nil, fmt.Errorf("pool: Opts.User must be set")
	}
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	newImpl := func(ctx context.Context) (poolImpl, error) {
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
		opts:       o,
		cfg:        cfg,
		all:        make(map[*holder]struct{}),
		idle:       make(chan *holder, cfg.Max),
		newImpl:    newImpl,
		reaperStop: make(chan struct{}),
		reaperDone: make(chan struct{}),
	}

	for i := 0; i < cfg.Min; i++ {
		h, err := p.createHolder(ctx)
		if err != nil {
			_ = p.Close(context.Background())
			return nil, fmt.Errorf("pool: creating min holder %d/%d: %w", i+1, cfg.Min, err)
		}
		p.idle <- h
	}

	if cfg.IdleTimeout > 0 {
		go p.reaper()
	} else {
		close(p.reaperDone) // no reaper goroutine; Close won't block on it
	}

	return p, nil
}

// createHolder builds one holder and registers it in `all`. Caller must
// NOT hold p.mu.
func (p *Pool) createHolder(ctx context.Context) (*holder, error) {
	impl, err := p.newImpl(ctx)
	if err != nil {
		return nil, err
	}
	h := &holder{impl: impl, lastUsed: time.Now()}
	p.mu.Lock()
	p.all[h] = struct{}{}
	p.mu.Unlock()
	return h, nil
}

// acquire returns a holder: an idle one if ready, else a freshly created
// one if under Max, else it blocks for an idle holder or ctx end.
func (p *Pool) acquire(ctx context.Context) (*holder, error) {
	select {
	case h := <-p.idle:
		return h, nil
	default:
	}

	// try to reserve a create slot atomically
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("pool: closed")
	}
	if len(p.all) < p.cfg.Max {
		h := &holder{lastUsed: time.Now()}
		p.all[h] = struct{}{} // reserve the slot now, before building
		p.mu.Unlock()

		impl, err := p.newImpl(ctx)
		if err != nil {
			p.mu.Lock()
			delete(p.all, h) // release the reservation on failure
			p.mu.Unlock()
			return nil, fmt.Errorf("pool: scaling up: %w", err)
		}
		h.impl = impl
		return h, nil
	}
	p.mu.Unlock()

	select {
	case h := <-p.idle:
		return h, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("pool: acquiring holder: %w", ctx.Err())
	}
}

// release returns a healthy holder to the idle set, or tears it down if
// the pool is closing.
func (p *Pool) release(h *holder) {
	h.lastUsed = time.Now()
	p.mu.Lock()
	if p.closed {
		delete(p.all, h)
		p.mu.Unlock()
		_ = h.impl.close(context.Background())
		return
	}
	p.mu.Unlock()
	p.idle <- h // cap Max, live <= Max, never blocks
}

// drop removes a holder from the pool and tears it down.
func (p *Pool) drop(h *holder) {
	p.mu.Lock()
	_, present := p.all[h]
	delete(p.all, h)
	p.mu.Unlock()
	if present {
		_ = h.impl.close(context.Background())
	}
}

// Run acquires a holder (scaling up if needed, blocking at Max), executes
// cmd, runs between-run hygiene, and returns the holder to the pool. A
// holder that dies during the run is dropped, not reused.
func (p *Pool) Run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	h, err := p.acquire(ctx)
	if err != nil {
		return res, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.dead != nil {
		p.drop(h)
		return res, fmt.Errorf("pool: acquired holder is dead: %w", h.dead)
	}

	res, runErr := h.impl.run(ctx, cmd, stdin, stdout, stderr)

	h.impl.cleanup()
	if err := h.impl.checkHolder(); err != nil {
		h.dead = err
		p.drop(h)
		if runErr == nil {
			runErr = fmt.Errorf("pool: holder died: %w", err)
		}
		return res, runErr
	}

	p.release(h)
	return res, runErr
}

// reaper periodically closes holders idle longer than IdleTimeout, never
// dropping the live count below Min.
func (p *Pool) reaper() {
	defer close(p.reaperDone)

	interval := p.cfg.IdleTimeout / 2
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-p.reaperStop:
			return
		case <-t.C:
			p.reapIdle()
		}
	}
}

// reapIdle closes idle holders past the timeout, keeping >= Min alive.
func (p *Pool) reapIdle() {
	now := time.Now()
	for {
		p.mu.Lock()
		live := len(p.all)
		p.mu.Unlock()
		if live <= p.cfg.Min {
			return
		}

		var h *holder
		select {
		case h = <-p.idle:
		default:
			return // nothing idle to reap
		}

		if now.Sub(h.lastUsed) < p.cfg.IdleTimeout {
			p.idle <- h // youngest-idle not old enough; put back and stop
			return
		}
		p.drop(h)
	}
}

// Close stops the reaper and tears down every holder. Idempotent.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	reaperRunning := p.cfg.IdleTimeout > 0
	p.mu.Unlock()

	// stop the reaper first so it can't pull/drop holders concurrently
	if reaperRunning {
		close(p.reaperStop)
	}
	<-p.reaperDone

	// now take the definitive holder set and clear it
	p.mu.Lock()
	holders := make([]*holder, 0, len(p.all))
	for h := range p.all {
		holders = append(holders, h)
	}
	p.all = make(map[*holder]struct{})
	p.mu.Unlock()

	// drain idle channel so any parked holders aren't double-handled
	// (they're in `holders` already; draining just empties the channel)
	for {
		select {
		case <-p.idle:
		default:
			goto closeAll
		}
	}
closeAll:

	var firstErr error
	for _, h := range holders {
		if err := h.impl.close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
