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

type dockerBackend struct{}

func (d *dockerBackend) Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	cli, err := client.New(client.FromEnv)
	if err != nil {
		return res, fmt.Errorf("connecting to socket: %w", err)
	}
	defer func() { _ = cli.Close() }()

	// pull if not local; errors ignored so local-only images work
	if _, err := cli.ImageInspect(ctx, o.Image); err != nil {
		_, _ = fmt.Fprintf(stderr, "pulling %s...\n", o.Image)
		if resp, err := cli.ImagePull(ctx, o.Image, client.ImagePullOptions{}); err == nil {
			_, _ = io.Copy(io.Discard, resp)
			_ = resp.Close()
		}
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:        o.Image,
			Cmd:          o.Cmd,
			Env:          o.Env,
			User:         o.User,
			WorkingDir:   "/work",
			AttachStdout: true,
			AttachStderr: true,
			AttachStdin:  o.WithStdin,
			OpenStdin:    o.WithStdin,
			StdinOnce:    o.WithStdin, // close container stdin when ours ends
		},
		HostConfig: &container.HostConfig{
			Runtime:        o.Runtime,
			NetworkMode:    "none",
			AutoRemove:     false, // we remove manually in the deferred cleanup
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    securityOpts(),
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
		return res, fmt.Errorf("create: %w", err)
	}
	id := created.ID

	// cleanup runs no matter how we exit — fresh context since ctx may be dead
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true})
	}()

	// Podman's Docker-compat API silently ignores HostConfig.Runtime;
	// a silent downgrade from a requested runtime (e.g. gVisor) is a
	// security failure, so verify the daemon honored the request.
	if o.Runtime != "" {
		insp, err := cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return res, fmt.Errorf("verifying runtime: %w", err)
		}
		if got := insp.Container.HostConfig.Runtime; got != o.Runtime {
			return res, fmt.Errorf("requested runtime %q but daemon assigned %q (podman's compat API ignores HostConfig.Runtime; configure the runtime daemon-side or use a Docker daemon)", o.Runtime, got)
		}
	}

	// attach before start so no output is missed
	att, err := cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
		Stdin:  o.WithStdin,
	})
	if err != nil {
		return res, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	// demux and stream output live
	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, att.Reader)
		outDone <- err
	}()

	// pump stdin if requested
	if o.WithStdin && stdin != nil {
		go func() {
			_, _ = io.Copy(att.Conn, stdin)
			_ = att.CloseWrite() // signal EOF to the container
		}()
	}

	started := time.Now()

	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return res, fmt.Errorf("start: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	wait := cli.ContainerWait(runCtx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})

	select {
	case w := <-wait.Result:
		res.ExitCode = int(w.StatusCode)
	case err := <-wait.Error:
		if runCtx.Err() != nil {
			res.TimedOut = true
			res.DurationMS = time.Since(started).Milliseconds()
			return res, fmt.Errorf("timed out after %s (container killed)", o.Timeout)
		}
		return res, fmt.Errorf("wait: %w", err)
	}

	res.DurationMS = time.Since(started).Milliseconds()

	// drain remaining buffered output
	select {
	case <-outDone:
	case <-time.After(2 * time.Second):
	}

	// OOM flag from inspect (reflects PID 1 only; exit 137 is the
	// more reliable signal)
	if insp, err := cli.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{}); err == nil && insp.Container.State != nil {
		res.OOMKilled = insp.Container.State.OOMKilled
	}

	return res, nil
}

func ptr[T any](v T) *T { return &v }
