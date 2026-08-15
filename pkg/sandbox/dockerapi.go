package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

type dockerBackend struct{}

func (d *dockerBackend) Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return res, fmt.Errorf("connecting to socket: %w", err)
	}
	defer cli.Close()

	// pull if not local; errors ignored so local-only images work
	if _, _, err := cli.ImageInspectWithRaw(ctx, o.Image); err != nil {
		fmt.Fprintf(stderr, "pulling %s...\n", o.Image)
		if rc, err := cli.ImagePull(ctx, o.Image, imagetypes.PullOptions{}); err == nil {
			io.Copy(io.Discard, rc)
			rc.Close()
		}
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
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
		&container.HostConfig{
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
		nil, nil, "")
	if err != nil {
		return res, fmt.Errorf("create: %w", err)
	}
	id := resp.ID

	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
	}()

	att, err := cli.ContainerAttach(ctx, id, container.AttachOptions{
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

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return res, fmt.Errorf("start: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	waitCh, errCh := cli.ContainerWait(runCtx, id, container.WaitConditionNotRunning)

	select {
	case w := <-waitCh:
		res.ExitCode = int(w.StatusCode)
	case err := <-errCh:
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

	if insp, err := cli.ContainerInspect(context.Background(), id); err == nil && insp.State != nil {
		res.OOMKilled = insp.State.OOMKilled
	}

	return res, nil
}

func ptr[T any](v T) *T { return &v }
