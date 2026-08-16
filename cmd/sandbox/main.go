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

	"podman-sandbox-runner/pkg/sandbox"
)

// version is overwritten at release build time via
// -ldflags "-X main.version={{.Version}}" (see .goreleaser.yaml).
var version = "dev"

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		fmt.Println("sandbox", version)
		return
	}

	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: sandbox run [flags] -- <command...> | sandbox version")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("run", flag.ExitOnError)
	image := fs.String("image", "docker.io/library/alpine:latest", "container image")
	timeout := fs.Duration("timeout", 30*time.Second, "hard execution timeout")
	mem := fs.Int64("mem", 256, "memory limit in MB")
	cpus := fs.Float64("cpus", 1.0, "CPU limit (cores)")
	user := fs.String("user", "65534:65534", "run as user (uid:gid); empty = image default")
	backend := fs.String("backend", "auto", "execution backend: auto|docker|k8s")
	netwait := fs.Duration("netwait", 0, "k8s only: delay start so network policy is enforced first (e.g. 3s)")
	stdinFlag := fs.Bool("i", false, "pipe stdin into the container")
	jsonFlag := fs.Bool("json", false, "emit result as JSON instead of streaming")
	var vols, envs multiFlag
	fs.Var(&vols, "v", "mount host file/dir read-only: /host/path:/container/path (repeatable)")
	fs.Var(&envs, "env", "environment variable KEY=VAL (repeatable)")
	fs.Parse(os.Args[2:])

	cmd := fs.Args()
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "no command given after flags")
		os.Exit(2)
	}

	binds := make([]string, 0, len(vols))
	for _, v := range vols {
		parts := strings.SplitN(v, ":", 3)
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "/") || !strings.HasPrefix(parts[1], "/") {
			fmt.Fprintf(os.Stderr, "bad -v %q: want /abs/host/path:/abs/container/path\n", v)
			os.Exit(2)
		}
		binds = append(binds, parts[0]+":"+parts[1]+":ro")
	}

	for _, e := range envs {
		if !strings.Contains(e, "=") {
			fmt.Fprintf(os.Stderr, "bad -env %q: want KEY=VAL\n", e)
			os.Exit(2)
		}
	}

	o := sandbox.Opts{
		Image:     *image,
		Cmd:       cmd,
		Timeout:   *timeout,
		NetWait:   *netwait,
		MemMB:     *mem,
		CPUs:      *cpus,
		User:      *user,
		Binds:     binds,
		Env:       envs,
		WithStdin: *stdinFlag,
		Backend:   *backend,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var stdin io.Reader
	if o.WithStdin {
		stdin = os.Stdin
	}

	if *jsonFlag {
		var outBuf, errBuf bytes.Buffer
		res, err := sandbox.Run(ctx, o, stdin, &outBuf, &errBuf)
		res.Stdout, res.Stderr = outBuf.String(), errBuf.String()
		if err != nil {
			res.Error = err.Error()
		}
		json.NewEncoder(os.Stdout).Encode(res)
		if err != nil {
			os.Exit(125)
		}
		os.Exit(0)
	}

	res, err := sandbox.Run(ctx, o, stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sandbox:", err)
		os.Exit(125)
	}
	os.Exit(res.ExitCode)
}
