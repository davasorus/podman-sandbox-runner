package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

func imagePullOptions() imagetypes.PullOptions { return imagetypes.PullOptions{} }

type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	OOMKilled  bool   `json:"oom_killed"`
	TimedOut   bool   `json:"timed_out"`
	Error      string `json:"error,omitempty"`
}

type Opts struct {
	Image     string
	Cmd       []string
	Timeout   time.Duration
	MemMB     int64
	CPUs      float64
	User      string
	Binds     []string
	WithStdin bool
	JSONMode  bool
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: sandbox run [flags] -- <command...>")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "docker.io/library/alpine:latest", "container image")
	timeout := fs.Duration("timeout", 30*time.Second, "hard execution timeout")
	mem := fs.Int64("mem", 256, "memory limit in MB")
	cpus := fs.Float64("cpus", 1.0, "CPU limit (cores)")
	user := fs.String("user", "65534:65534", "run as user (uid:gid); empty = image default")
	stdinFlag := fs.Bool("i", false, "pipe stdin into the container")
	jsonFlag := fs.Bool("json", false, "emit result as JSON instead of streaming")
	var vols multiFlag
	fs.Var(&vols, "v", "mount host file/dir read-only: /host/path:/container/path (repeatable)")
	fs.Parse(os.Args[2:])

	cmd := fs.Args()
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "no command given after flags")
		os.Exit(2)
	}

	// normalize mounts: absolute paths required, read-only enforced
	binds := make([]string, 0, len(vols))
	for _, v := range vols {
		parts := strings.SplitN(v, ":", 3)
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "/") || !strings.HasPrefix(parts[1], "/") {
			fmt.Fprintf(os.Stderr, "bad -v %q: want /abs/host/path:/abs/container/path\n", v)
			os.Exit(2)
		}
		binds = append(binds, parts[0]+":"+parts[1]+":ro")
	}

	o := Opts{
		Image:     *image,
		Cmd:       cmd,
		Timeout:   *timeout,
		MemMB:     *mem,
		CPUs:      *cpus,
		User:      *user,
		Binds:     binds,
		WithStdin: *stdinFlag,
		JSONMode:  *jsonFlag,
	}

	res, err := run(o)

	if o.JSONMode {
		if err != nil {
			res.Error = err.Error()
		}
		json.NewEncoder(os.Stdout).Encode(res)
		if err != nil {
			os.Exit(125)
		}
		os.Exit(0) // container exit code lives in the JSON
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(125)
	}
	os.Exit(res.ExitCode)
}

func run(o Opts) (Result, error) {
	var res Result

	// ctrl-C cancels everything; cleanup still runs
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return res, fmt.Errorf("connecting to socket: %w", err)
	}
	defer cli.Close()

	// pull if not present; errors deliberately ignored so local-only images still work
	if rc, err := cli.ImagePull(ctx, o.Image, imagePullOptions()); err == nil {
		io.Copy(io.Discard, rc)
		rc.Close()
	}

	resp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:        o.Image,
			Cmd:          o.Cmd,
			User:         o.User,
			WorkingDir:   "/work",
			AttachStdout: true,
			AttachStderr: true,
			AttachStdin:  o.WithStdin,
			OpenStdin:    o.WithStdin,
			StdinOnce:    o.WithStdin, // close container stdin when ours ends
		},
		&container.HostConfig{
			NetworkMode:    "none",
			AutoRemove:     false, // we remove manually in the deferred cleanup
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			Binds:          o.Binds,
			Tmpfs:          map[string]string{"/work": "rw,size=64m", "/tmp": "rw,size=16m"},
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
		Stdin:  o.WithStdin,
	})
	if err != nil {
		return res, fmt.Errorf("attach: %w", err)
	}
	defer att.Close()

	// output goes to the terminal, or into buffers in JSON mode
	var stdoutBuf, stderrBuf bytes.Buffer
	outW, errW := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if o.JSONMode {
		outW, errW = &stdoutBuf, &stderrBuf
	}

	outDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(outW, errW, att.Reader)
		outDone <- err
	}()

	// pump stdin if requested
	if o.WithStdin {
		go func() {
			io.Copy(att.Conn, os.Stdin)
			att.CloseWrite() // signal EOF to the container
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
			res.Stdout, res.Stderr = stdoutBuf.String(), stderrBuf.String()
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
	res.Stdout, res.Stderr = stdoutBuf.String(), stderrBuf.String()

	// OOM flag from inspect
	if insp, err := cli.ContainerInspect(context.Background(), id); err == nil && insp.State != nil {
		res.OOMKilled = insp.State.OOMKilled
	}

	return res, nil
}

func ptr[T any](v T) *T { return &v }
