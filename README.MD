# podman-sandbox-runner

Run untrusted commands and code in ephemeral, locked-down containers.
Pipe code in, get output out — nothing escapes. Supports docker, podman,
and Kubernetes backends behind one CLI and one Go library, with optional
gVisor runtime selection.

```sh
echo 'print(21 * 2)' | sandbox run -json -i -image docker.io/library/python:3-alpine -- python3 -
{"exit_code":0,"stdout":"42\n","stderr":"","duration_ms":278,"oom_killed":false,"timed_out":false}
```

## Backends

|                   | `docker` (default)                                       | `k8s`                                                           |
| ----------------- | -------------------------------------------------------- | --------------------------------------------------------------- |
| Works against     | Docker Engine and podman (compat API) via `DOCKER_HOST`  | Any cluster via kubeconfig (`KUBECONFIG` or `~/.kube/config`)  |
| Network isolation | **Structural** — `NetworkMode: none`; the network never exists | Policy-based — NetworkPolicy created automatically; see caveats |
| stdout/stderr     | Separated, streamed live                                  | Separated, streamed live (exec-based)                            |
| `-v` mounts       | Yes (read-only enforced)                                  | Not supported (host paths would be node paths)                   |
| OOM detection     | Best-effort (`exit 137` is the reliable signal)           | `exit 137` heuristic (same as docker)                            |
| Alternate runtime | `-runtime` sets the OCI runtime (fail-closed, see below)  | `-runtime` sets the RuntimeClass (fail-closed at admission)      |

For hostile code where network isolation must be absolute, prefer the
docker/podman backend: a network that never exists beats a policy that
denies. See the k8s caveats below.

## Security envelope

Every run gets:

- **No network** — structural on docker/podman; deny-all NetworkPolicy on k8s
- **Read-only rootfs** — writes only to memory-backed scratch at `/work`
  (64M) and `/tmp` (16M), destroyed with the container
- **Runs as nobody** (65534:65534) by default; all capabilities dropped,
  no privilege escalation
- **Memory limit** (default 256M), pids limit (128, docker/podman), CPU
  limit (default 1 core)
- **Hard timeout** (default 30s) — container/pod force-removed on expiry
- **Guaranteed cleanup** on every exit path, including ctrl-C
- **k8s extra**: no service account token mounted, no service links
- **Optional gVisor** — `-runtime` interposes a userspace kernel between
  the workload and the host (see Alternate runtimes)

## Usage

```
sandbox run [flags] -- <command...>
sandbox version

  -backend string   auto|docker|k8s (default "auto" = docker API)
  -image string     container image (default docker.io/library/alpine:latest)
  -timeout duration hard execution timeout (default 30s)
  -mem int          memory limit in MB (default 256)
  -cpus float       CPU limit in cores (default 1)
  -user string      uid:gid (default "65534:65534"); empty = image default
  -runtime string   alternate OCI runtime (docker backend, e.g. "runsc") or
                    RuntimeClass name (k8s backend, e.g. "gvisor"); empty
                    uses the backend default
  -v path:path      mount host file/dir read-only (repeatable; docker/podman only)
  -env KEY=VAL      environment variable (repeatable)
  -i                pipe stdin into the container
  -netwait duration k8s only: init delay so network policy is enforced
                    before the command runs (e.g. 3s; default 0)
  -json             emit structured result instead of streaming
```

Without `-json`, output streams live and the process exit code mirrors
the container's. With `-json`, one JSON object is printed; the container
exit code is in the payload and the process exits 0 (125 on
sandbox-level errors, with `error` set).

### Examples

```sh
# stream a command
sandbox run -- sh -c 'echo hello; echo oops >&2'

# piped code in a runtime image
echo 'console.log(6*7)' | sandbox run -i -image docker.io/library/node:alpine -- node -

# run a script file (docker/podman)
sandbox run -v /home/me/script.py:/work/script.py -image docker.io/library/python:3-alpine -- python3 /work/script.py

# machine-consumable result
sandbox run -json -timeout 5s -- sh -c 'exit 3'

# same code on kubernetes, with the network race closed
echo 'print(6*7)' | sandbox run -backend k8s -netwait 3s -json -i -image docker.io/library/python:3-alpine -- python3 -

# gVisor isolation on kubernetes
sandbox run -backend k8s -runtime gvisor -- uname -a
```

## Library use

```go
import "github.com/davasorus/podman-sandbox-runner/pkg/sandbox"

res, err := sandbox.Run(ctx, sandbox.Opts{
    Image:   "docker.io/library/python:3-alpine",
    Cmd:     []string{"python3", "-"},
    Timeout: 30 * time.Second,
    MemMB:   256, CPUs: 1, User: "65534:65534",
    WithStdin: true,
}, strings.NewReader(code), &stdout, &stderr)
```

`sandbox.Backend` is an interface; `New(opts)` selects the implementation
from `Opts.Backend`. API documentation is on pkg.go.dev.

## Warm pools (library only)

For latency-sensitive callers making many runs, `sandbox.Pool` keeps a
holder container warm and executes each run via the exec API — warm
runs complete in well under a second versus seconds for cold starts:

```go
pool, err := sandbox.NewPool(ctx, opts)   // opts.Cmd ignored; opts.User required
defer pool.Close(ctx)
res, err := pool.Run(ctx, []string{"python3", "-"}, stdin, &out, &errBuf)
```

**Security tradeoff, stated plainly:** runs in one pool share a
container. Between runs the pool kills the workload user's processes
and clears `/work` and `/tmp`, but this hygiene is best-effort — runs
in one pool must belong to the same trust domain. For mutually
untrusted callers, use one-shot `Run` or separate pools. The holder
runs as the image default user with an init process (PID 1 reaping);
workloads exec as `Opts.User`. If a workload kills the holder (e.g. a
cgroup-wide OOM), the pool fails closed and reports itself unusable
rather than silently respawning. Docker/podman backend only.

## Connection

**docker/podman**: standard Docker client env vars.

```sh
# rootless podman on Linux
export DOCKER_HOST=unix:///run/user/$UID/podman/podman.sock
```

On Windows with a running podman machine, the client defaults to the
`docker_engine` named pipe — runs land on the machine.

**k8s**: `KUBECONFIG`, falling back to `~/.kube/config`, falling back to
in-cluster config. Pods are created in `default` or `$SANDBOX_NAMESPACE`.

## Alternate runtimes (gVisor)

`-runtime` selects a stronger isolation runtime where the backend
supports it, upgrading the "not a VM" caveat into an option:

```sh
# k8s with a "gvisor" RuntimeClass installed (runsc + containerd shim)
sandbox run -backend k8s -runtime gvisor -- uname -a
# Linux ... 4.19.0-gvisor ...   <- syscalls served by the gVisor sentry
```

Behavior and caveats, verified against k3s + runsc:

- **Fail-closed everywhere.** On k8s, a nonexistent RuntimeClass is
  rejected at pod admission. On the docker backend, the tool inspects
  the created container and errors if the daemon did not honor the
  requested runtime — because **podman's Docker-compat API silently
  ignores `HostConfig.Runtime`**, and a silent downgrade from gVisor to
  runc would be a security failure. Podman users who want gVisor must
  configure it daemon-side; note also that **rootless podman cannot run
  runsc at all** (cgroup delegation), so a rootful daemon is required.
- **Memory limits behave differently under gVisor.** Exceeding the
  limit may stall the workload rather than OOM-kill it: no exit 137, no
  `oom_killed`, the process hangs. Always set a `-timeout` — under
  gVisor it is the effective resource backstop.
- NetworkPolicy isolation applies to gVisor pods the same as native
  ones (verified).
- The RuntimeClass/runtime must be installed on the cluster or host;
  this tool does not install it.

## Kubernetes caveats

Read these before pointing the k8s backend at hostile code.

- **Network isolation is policy-based, and enforcement depends on the
  CNI.** The tool creates a deny-all NetworkPolicy
  (`sandbox-runner-deny-all`) per namespace, scoped to
  `app=sandbox-runner`. On clusters whose CNI enforces NetworkPolicy
  (k3s's bundled kube-router controller, Calico, Cilium, ...) it works.
  On clusters with a non-enforcing CNI it is **silently decorative** —
  the tool cannot verify enforcement.
- **Enforcement is not instantaneous.** iptables-based controllers
  program per-pod rules reactively, a few seconds after pod start.
  Hostile code that egresses in its first moments can win that race.
  `-netwait 3s` closes the window by delaying execution, at the cost of
  added latency.
- **The image must contain a `sleep` binary.** The pod runs sleep as a
  holder process and the command executes via the exec API (this is
  what enables separated, live streams). Scratch-style images without a
  shell or coreutils will not work on this backend.
- `-v` host mounts are rejected — a "host" path on k8s is a node path,
  which is rarely what you mean and dangerous when it is.

## Other caveats

- **`oom_killed` is best-effort** — on the docker backend it reflects
  PID 1 only; on the k8s backend it is inferred from exit code 137.
  Treat **exit code 137** as the reliable memory-kill signal (except
  under gVisor — see Alternate runtimes).
- Scratch tmpfs uses `mode=1777` so the non-root sandbox user can write
  to `/work` and `/tmp` on all backends.
- Image pulls happen implicitly on first use; pull errors are ignored so
  local-only images work — a typo'd image surfaces as a create error.
  On k8s, pulls happen in the kubelet with no progress output.
- Not a VM. Container isolation is kernel-level; hostile-kernel-exploit
  threat models need a stronger boundary (gVisor via `-runtime`, Kata,
  or a real VM).

## Development

```sh
go build -o sandbox ./cmd/sandbox
go test -v -timeout 300s ./...     # docker-backend suite; needs a docker/podman socket
```

The suite covers both backends: the docker-backend tests run against
Docker on GitHub runners, and the k8s-backend tests run against a k3s
cluster installed in CI. The k8s tests are gated behind
`SANDBOX_K8S_TEST=1`; the gVisor runtime test is additionally gated
behind `SANDBOX_GVISOR_TEST=1` and requires a `gvisor` RuntimeClass —
it is not run in CI.