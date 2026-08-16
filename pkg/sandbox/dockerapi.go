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

	// NOTE: confirm against `go doc client.New` / `client.FromEnv` output
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return res, fmt.Errorf("connecting to socket: %w", err)
	}
	defer cli.Close()

	// pull if not local; errors ignored so local-only images work
	if _, err := cli.ImageInspect(ctx, o.Image); err != nil {
		fmt.Fprintf(stderr, "pulling %s...\n", o.Image)
		if resp, err := cli.ImagePull(ctx, o.Image, client.ImagePullOptions{}); err == nil {
			io.Copy(io.Discard, resp)
			resp.Close()
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
			StdinOnce:    o.WithStdin,
		},
		HostConfig: &container.HostConfig{
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
		return res, fmt.Errorf("create: %w", err)
	}
	id := created.ID

	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cli.ContainerRemove(rmCtx, id, client.ContainerRemoveOptions{Force: true})
	}()

	// attach before start so no output is missed
	att, err := cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		// NOTE: field names pending `go doc client.ContainerAttachOptions`
		Stream: true,
		Stdout: true,
		Stderr: true,
		Stdin:  o.WithStdin,
	})
	if err != nil {
		return res, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(stdout, stderr, att.Reader)
		outDone <- err
	}()

	if o.WithStdin && stdin != nil {
		go func() {
			io.Copy(att.Conn, stdin)
			att.CloseWrite()
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

	select {
	case <-outDone:
	case <-time.After(2 * time.Second):
	}

	// OOM flag from inspect
	if insp, err := cli.ContainerInspect(context.Background(), id, client.ContainerInspectOptions{}); err == nil && insp.Container.State != nil {
		res.OOMKilled = insp.Container.State.OOMKilled
	}

	return res, nil
}

func ptr[T any](v T) *T { return &v }
