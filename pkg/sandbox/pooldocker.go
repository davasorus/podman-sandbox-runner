package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type dockerPool struct {
	cli  *client.Client
	id   string
	opts Opts
}

func newDockerPool(ctx context.Context, o Opts) (*dockerPool, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("pool: connecting to socket: %w", err)
	}

	if _, err := cli.ImageInspect(ctx, o.Image); err != nil {
		if resp, err := cli.ImagePull(ctx, o.Image, client.ImagePullOptions{}); err == nil {
			_, _ = io.Copy(io.Discard, resp)
			_ = resp.Close()
		}
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      o.Image,
			Cmd:        []string{"sleep", "infinity"},
			WorkingDir: "/work",
		},
		HostConfig: &container.HostConfig{
			Runtime:        o.Runtime,
			Init:           ptr(true),
			NetworkMode:    "none",
			AutoRemove:     false,
			ReadonlyRootfs: true,
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
		_ = cli.Close()
		return nil, fmt.Errorf("pool: create holder: %w", err)
	}
	id := created.ID

	fail := func(e error) (*dockerPool, error) {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true})
		_ = cli.Close()
		return nil, e
	}

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

	return &dockerPool{cli: cli, id: id, opts: o}, nil
}

func (d *dockerPool) run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	runCtx, cancel := context.WithTimeout(ctx, d.opts.Timeout)
	defer cancel()

	withStdin := stdin != nil

	ec, err := d.cli.ExecCreate(runCtx, d.id, client.ExecCreateOptions{
		User:         d.opts.User,
		Cmd:          cmd,
		Env:          d.opts.Env,
		WorkingDir:   "/work",
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  withStdin,
	})
	if err != nil {
		return res, fmt.Errorf("pool: exec create: %w", err)
	}

	att, err := d.cli.ExecAttach(runCtx, ec.ID, client.ExecAttachOptions{})
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
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
		}()
	}

	select {
	case <-outDone:
	case <-runCtx.Done():
	}
	res.DurationMS = time.Since(started).Milliseconds()

	if runCtx.Err() != nil {
		res.TimedOut = true
		return res, fmt.Errorf("pool: timed out after %s (workload killed)", d.opts.Timeout)
	}

	insp, err := d.cli.ExecInspect(context.Background(), ec.ID, client.ExecInspectOptions{})
	if err != nil {
		return res, fmt.Errorf("pool: exec inspect: %w", err)
	}
	res.ExitCode = insp.ExitCode
	if res.ExitCode == 137 {
		res.OOMKilled = true
	}
	return res, nil
}

// cleanup runs AS the workload user: a uid can always signal its own
// processes. kill -9 -1 kills everything that uid can signal — the
// root-side holder is untouchable, and the cleanup shell dies last.
func (d *dockerPool) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ec, err := d.cli.ExecCreate(ctx, d.id, client.ExecCreateOptions{
		User:         d.opts.User,
		Cmd:          []string{"sh", "-c", "rm -rf /work/* /tmp/* 2>/dev/null; kill -9 -1"},
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return
	}
	att, err := d.cli.ExecAttach(ctx, ec.ID, client.ExecAttachOptions{})
	if err != nil {
		return
	}
	defer att.Close()
	_, _ = io.Copy(io.Discard, att.Reader)
}

func (d *dockerPool) checkHolder() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insp, err := d.cli.ContainerInspect(ctx, d.id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("holder inspect failed: %w", err)
	}
	if insp.Container.State == nil || !insp.Container.State.Running {
		return fmt.Errorf("holder died (possibly OOM-killed by a workload)")
	}
	return nil
}

func (d *dockerPool) close(ctx context.Context) error {
	rmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := d.cli.ContainerRemove(rmCtx, d.id, client.ContainerRemoveOptions{Force: true})
	_ = d.cli.Close()
	return err
}
