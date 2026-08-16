package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Pool keeps a single warm holder container alive and executes runs
// inside it via the exec API, trading inter-run isolation for latency.
//
// SECURITY TRADEOFF: unlike one-shot Run, consecutive runs in a pool
// share a container. Between runs the pool kills the workload user's
// processes and clears /work and /tmp, but this cleanup is best-effort:
// runs in one pool must belong to the same trust domain. For mutually
// untrusted callers, use one-shot Run or separate pools.
//
// The holder process runs as the image's default user; workloads exec
// as Opts.User (nobody by default), which is what allows the root-side
// cleanup to reap them. Opts.User must therefore be non-empty and
// different from the image default for the isolation split to hold.
//
// Runs are serialized: Pool.Run holds an internal mutex. For
// concurrency, create multiple pools.
//
// Only the docker backend is supported. Opts.Cmd is ignored (each Run
// supplies its own command); Opts.Timeout applies per run.
type Pool struct {
	mu     sync.Mutex
	cli    *client.Client
	id     string // holder container ID
	opts   Opts
	broken error // set permanently if the holder dies
	closed bool
}

// NewPool creates and starts the holder container. The caller must
// Close the pool to remove it.
func NewPool(ctx context.Context, o Opts) (*Pool, error) {
	switch o.Backend {
	case "", "auto", "docker":
	default:
		return nil, fmt.Errorf("pool: backend %q not supported (docker only)", o.Backend)
	}
	if o.User == "" {
		return nil, fmt.Errorf("pool: Opts.User must be set (workloads exec as this user; the holder runs as the image default)")
	}

	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("pool: connecting to socket: %w", err)
	}

	// pull if not local; errors ignored so local-only images work
	if _, err := cli.ImageInspect(ctx, o.Image); err != nil {
		if resp, err := cli.ImagePull(ctx, o.Image, client.ImagePullOptions{}); err == nil {
			io.Copy(io.Discard, resp)
			resp.Close()
		}
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: o.Image,
			// holder: image-default user, sleeps forever; workloads
			// exec as o.User so root-side cleanup can reap them
			Cmd:        []string{"sleep", "infinity"},
			WorkingDir: "/work",
		},
		HostConfig: &container.HostConfig{
			Runtime:        o.Runtime,
			NetworkMode:    "none",
			AutoRemove:     false,
			ReadonlyRootfs: true,
			Init:           ptr(true),
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Binds:          o.Binds,
			Tmpfs: map[string]string{
				"/work": "rw,size=64m,mode=1777",
				"/tmp":  "rw,size=16m,mode=1777",
			},
			Resources: container.Resources{
				Memory:    o.MemMB * 1024 * 1024,
				NanoCPUs:  int64(o.CPUs * 1e9),
				PidsLimit: ptr(int64(128)),
			},
		},
	})
	if err != nil {
		cli.Close()
		return nil, fmt.Errorf("pool: create holder: %w", err)
	}
	id := created.ID

	fail := func(e error) (*Pool, error) {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true})
		cli.Close()
		return nil, e
	}

	// runtime fail-closed check, same rationale as the one-shot backend
	if o.Runtime != "" {
		insp, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return fail(fmt.Errorf("pool: verifying runtime: %w", err))
		}
		if got := insp.Container.HostConfig.Runtime; got != o.Runtime {
			return fail(fmt.Errorf("pool: requested runtime %q but daemon assigned %q", o.Runtime, got))
		}
	}

	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("pool: start holder: %w", err))
	}

	return &Pool{cli: cli, id: id, opts: o}, nil
}

// Run executes cmd in the warm holder and returns when it exits or
// Opts.Timeout expires. Runs are serialized. stdin may be nil.
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

	runCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout)
	defer cancel()

	withStdin := stdin != nil

	ec, err := p.cli.ExecCreate(runCtx, p.id, client.ExecCreateOptions{
		User:         p.opts.User,
		Cmd:          cmd,
		Env:          p.opts.Env,
		WorkingDir:   "/work",
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  withStdin,
	})
	if err != nil {
		return res, fmt.Errorf("pool: exec create: %w", err)
	}

	// Attaching to an exec starts it (classic Docker API semantics).
	att, err := p.cli.ExecAttach(runCtx, ec.ID, client.ExecAttachOptions{})
	if err != nil {
		return res, fmt.Errorf("pool: exec attach: %w", err)
	}
	defer att.Close()

	started := time.Now()

	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, att.Reader)
		outDone <- err
	}()

	if withStdin {
		go func() {
			io.Copy(att.Conn, stdin)
			att.CloseWrite()
		}()
	}

	// the stream closing is the exec finishing (or the context dying)
	select {
	case <-outDone:
	case <-runCtx.Done():
	}
	res.DurationMS = time.Since(started).Milliseconds()

	if runCtx.Err() != nil {
		res.TimedOut = true
		// reap the runaway workload so the pool stays usable
		p.cleanup()
		if err := p.checkHolder(); err != nil {
			p.broken = err
		}
		return res, fmt.Errorf("pool: timed out after %s (workload killed)", p.opts.Timeout)
	}

	insp, err := p.cli.ExecInspect(context.Background(), ec.ID, client.ExecInspectOptions{})
	if err != nil {
		return res, fmt.Errorf("pool: exec inspect: %w", err)
	}
	res.ExitCode = insp.ExitCode
	if res.ExitCode == 137 {
		res.OOMKilled = true // heuristic, same caveat as elsewhere
	}

	// between-runs hygiene: reap workload processes, clear scratch
	p.cleanup()

	// an OOM or hostile workload can take the holder down with it;
	// fail closed rather than silently respawning
	if err := p.checkHolder(); err != nil {
		p.broken = err
		return res, fmt.Errorf("pool: %w (create a new pool)", err)
	}

	return res, nil
}

// cleanup clears scratch space and reaps the workload user's processes.
// It runs AS the workload user: a uid can always signal its own
// processes (no CAP_KILL needed — we drop all capabilities, so a
// root-side reap would be powerless against another uid). `kill -9 -1`
// kills everything the workload uid can signal, including this cleanup
// shell itself last — its exit code is meaningless and ignored.
// Best-effort by design; the holder-liveness check is the backstop.
func (p *Pool) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ec, err := p.cli.ExecCreate(ctx, p.id, client.ExecCreateOptions{
		User:         p.opts.User,
		Cmd:          []string{"sh", "-c", "rm -rf /work/* /tmp/* 2>/dev/null; kill -9 -1"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return
	}
	att, err := p.cli.ExecAttach(ctx, ec.ID, client.ExecAttachOptions{})
	if err != nil {
		return
	}
	defer att.Close()
	io.Copy(io.Discard, att.Reader) // TEMP: was io.Copy(io.Discard, ...
}

// checkHolder verifies the holder container is still running.
func (p *Pool) checkHolder() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insp, err := p.cli.ContainerInspect(ctx, p.id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("holder inspect failed: %w", err)
	}
	if insp.Container.State == nil || !insp.Container.State.Running {
		return fmt.Errorf("holder died (possibly OOM-killed by a workload)")
	}
	return nil
}

// Close removes the holder container. Idempotent.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	rmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := p.cli.ContainerRemove(rmCtx, p.id, client.ContainerRemoveOptions{Force: true})
	p.cli.Close()
	return err
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
