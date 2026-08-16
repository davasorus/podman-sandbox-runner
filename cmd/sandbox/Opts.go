package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
)

// sandboxFlags holds pointers to the sandbox-configuration flag values
// shared by `run` and `serve`. Register with registerSandboxFlags, then
// call buildOpts after fs.Parse to get a validated sandbox.Opts.
type sandboxFlags struct {
	image   *string
	timeout *time.Duration
	netwait *time.Duration
	mem     *int64
	cpus    *float64
	user    *string
	backend *string
	runtime *string
	vols    multiFlag
	envs    multiFlag
}

// registerSandboxFlags declares the flags common to `run` and `serve` on
// fs and returns handles to their values.
func registerSandboxFlags(fs *flag.FlagSet) *sandboxFlags {
	sf := &sandboxFlags{
		image:   fs.String("image", "docker.io/library/alpine:latest", "container image"),
		timeout: fs.Duration("timeout", 30*time.Second, "hard execution timeout"),
		netwait: fs.Duration("netwait", 0, "k8s only: delay start so network policy is enforced first (e.g. 3s)"),
		mem:     fs.Int64("mem", 256, "memory limit in MB"),
		cpus:    fs.Float64("cpus", 1.0, "CPU limit (cores)"),
		user:    fs.String("user", "65534:65534", "run as user (uid:gid); empty = image default"),
		backend: fs.String("backend", "auto", "execution backend: auto|docker|k8s"),
		runtime: fs.String("runtime", "", "alternate OCI runtime (docker: e.g. runsc) or RuntimeClass (k8s)"),
	}
	fs.Var(&sf.vols, "v", "mount host file/dir read-only: /host/path:/container/path (repeatable)")
	fs.Var(&sf.envs, "env", "environment variable KEY=VAL (repeatable)")
	return sf
}

// buildOpts validates the parsed flags and assembles a sandbox.Opts. cmd
// is the command+args (empty for `serve`, which supplies commands per
// request). withStdin sets Opts.WithStdin. Returns an error rather than
// exiting, so each caller reports it in its own idiom.
func (sf *sandboxFlags) buildOpts(cmd []string, withStdin bool) (sandbox.Opts, error) {
	binds := make([]string, 0, len(sf.vols))
	for _, v := range sf.vols {
		parts := strings.SplitN(v, ":", 3)
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "/") || !strings.HasPrefix(parts[1], "/") {
			return sandbox.Opts{}, fmt.Errorf("bad -v %q: want /abs/host/path:/abs/container/path", v)
		}
		binds = append(binds, parts[0]+":"+parts[1]+":ro")
	}
	for _, e := range sf.envs {
		if !strings.Contains(e, "=") {
			return sandbox.Opts{}, fmt.Errorf("bad -env %q: want KEY=VAL", e)
		}
	}
	return sandbox.Opts{
		Image:     *sf.image,
		Cmd:       cmd,
		Timeout:   *sf.timeout,
		NetWait:   *sf.netwait,
		MemMB:     *sf.mem,
		CPUs:      *sf.cpus,
		User:      *sf.user,
		Binds:     binds,
		Env:       sf.envs,
		WithStdin: withStdin,
		Backend:   *sf.backend,
		Runtime:   *sf.runtime,
	}, nil
}
