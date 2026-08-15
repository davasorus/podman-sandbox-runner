package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func imagePullOptions() imagetypes.PullOptions { return imagetypes.PullOptions{} }

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: sandbox run [flags] -- <command...>")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "docker.io/library/alpine:latest", "container image")
	timeout := fs.Duration("timeout", 30*time.Second, "hard execution timeout")
	mem := fs.Int64("mem", 256, "memory limit in MB")
	stdinFlag := fs.Bool("i", false, "pipe stdin into the container")
	fs.Parse(os.Args[2:])

	cmd := fs.Args()
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "no command given after flags")
		os.Exit(2)
	}

	exitCode, err := run(*image, cmd, *timeout, *mem, *stdinFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(125)
	}
	os.Exit(exitCode)
}

func run(image string, cmd []string, timeout time.Duration, memMB int64, withStdin bool) (int, error) {
	// ctrl-C cancels everything; cleanup still runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return 0, fmt.Errorf("connecting to socket: %w", err)
	}
	defer cli.Close()

	// pull if not present; errors deliberately ignored so local-only images still work
	if rc, err := cli.ImagePull(ctx, image, imagePullOptions()); err == nil {
		io.Copy(io.Discard, rc)
		rc.Close()
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        image,
			Cmd:          cmd,
			WorkingDir:   "/work",
			AttachStdout: true,
			AttachStderr: true,
			AttachStdin:  withStdin,
			OpenStdin:    withStdin,
			StdinOnce:    withStdin, // close container stdin when ours ends
		},
		&container.HostConfig{
			NetworkMode:    "none",
			AutoRemove:     false, // we remove manually in the deferred cleanup
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Tmpfs:          map[string]string{"/work": "rw,size=64m", "/tmp": "rw,size=16m"},
			Resources: container.Resources{
				Memory:    memMB * 1024 * 1024,
				PidsLimit: ptr(int64(128)),
			},
		},
		nil, nil, "")
	if err != nil {
		return 0, fmt.Errorf("create: %w", err)
	}
	id := resp.ID

	// cleanup runs no matter how we exit — fresh context since ctx may be dead
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true})
	}()

	// attach before start so no output is missed
	att, err := cli.ContainerAttach(ctx, id, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
		Stdin:  withStdin,
	})
	if err != nil {
		return 0, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	// demux and stream output live
	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(os.Stdout, os.Stderr, att.Reader)
		outDone <- err
	}()

	// pump stdin if requested
	if withStdin {
		go func() {
			io.Copy(att.Conn, os.Stdin)
			att.CloseWrite() // signal EOF to the container
		}()
	}

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	waitCh, errCh := cli.ContainerWait(runCtx, id, container.WaitConditionNotRunning)

	var exitCode int
	select {
	case w := <-waitCh:
		exitCode = int(w.StatusCode)
	case err := <-errCh:
		if runCtx.Err() != nil {
			return 0, fmt.Errorf("timed out after %s (container killed)", timeout)
		}
		return 0, fmt.Errorf("wait: %w", err)
	}

	// drain remaining buffered output before returning
	select {
	case <-outDone:
	case <-time.After(2 * time.Second):
	}

	return exitCode, nil
}

func ptr[T any](v T) *T { return &v }
