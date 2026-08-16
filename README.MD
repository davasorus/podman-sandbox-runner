# podman-sandbox-runner

Run untrusted commands and code in ephemeral, locked-down containers.
Pipe code in, get output out — nothing escapes. Supports docker, podman,
and Kubernetes backends behind one CLI and one Go library.

```sh
echo 'print(21 * 2)' | sandbox run -json -i -image docker.io/library/python:3-alpine -- python3 -
{"exit_code":0,"stdout":"42\n","stderr":"","duration_ms":278,"oom_killed":false,"timed_out":false}
```

## Backends

|                   | `docker` (default)                                    | `k8s`                                                          |
| ----------------- | ----------------------------------------------------- | -------------------------------------------------------------- |
| Works against     | Docker Engine and podman (compat API) via `DOCKER_HOST` | Any cluster via kubeconfig (`KUBECONFIG` or `~/.kube/config`) |
| Network isolation | **Structural** — `NetworkMode: none`; the network never exists | Policy-based — NetworkPolicy created automatically; see caveats |
| stdout/stderr     | Separated, streamed live                               | Merged into stdout, delivered after exit                        |
| `-v` mounts       | Yes (read-only enforced)                               | Not supported (host paths would be node paths)                  |
| OOM detection     | Best-effort (`exit 137` is the reliable signal)        | Reliable (`oom_killed` from container status)                   |

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

## Usage

```
sandbox run [flags] -- <command...>

  -backend string   auto|docker|k8s (default "auto" = docker API)
  -image string     container image (default docker.io/library/alpine:latest)
  -timeout duration hard execution timeout (default 30s)
  -mem int          memory limit in MB (default 256)
  -cpus float       CPU limit in cores (default 1)
  -user string      uid:gid (default "65534:65534"); empty = image default
  -v path:path      mount host file/dir read-only (repeatable; docker/podman only)
  -env KEY=VAL      environment variable (repeatable)
  -i                pipe stdin into the container
  -netwait duration k8s only: init-container delay so network policy is
                    enforced before the command runs (e.g. 3s; default 0)
  -json             emit structured result instead of streaming
```

Without `-json`, output streams live (docker/podman) and the process exit
code mirrors the container's. With `-json`, one JSON object is printed;
the container exit code is in the payload and the process exits 0 (125 on
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
```

## Library use

```go
import "podman-sandbox-runner/pkg/sandbox"

res, err := sandbox.Run(ctx, sandbox.Opts{
    Image:   "docker.io/library/python:3-alpine",
    Cmd:     []string{"python3", "-"},
    Timeout: 30 * time.Second,
    MemMB:   256, CPUs: 1, User: "65534:65534",
    WithStdin: true,
}, strings.NewReader(code), &stdout, &stderr)
```

`sandbox.Backend` is an interface; `New(opts)` selects the implementation
from `Opts.Backend`.

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
  `-netwait 3s` closes the window by delaying the sandbox container
  behind an init container, at the cost of added latency.
- **stdout and stderr are merged** (k8s logs don't separate streams);
  the JSON `stderr` field is always empty, and output arrives after
  exit rather than streaming.
- `-v` host mounts are rejected — a "host" path on k8s is a node path,
  which is rarely what you mean and dangerous when it is.

## Other caveats

- **`oom_killed` is best-effort on docker/podman** — the flag reflects
  PID 1 only. Treat **exit code 137** as the reliable memory-kill signal.
  (On k8s the flag is reliable.)
- Scratch tmpfs uses `mode=1777` so the non-root sandbox user can write
  to `/work` and `/tmp` on all backends.
- Image pulls happen implicitly on first use; pull errors are ignored so
  local-only images work — a typo'd image surfaces as a create error.
  On k8s, pulls happen in the kubelet with no progress output.
- Not a VM. Container isolation is kernel-level; hostile-kernel-exploit
  threat models need stronger isolation (gVisor, Kata, a VM).

## Development

```sh
go build -o sandbox ./cmd/sandbox
go test -v -timeout 300s ./...     # integration tests; needs a docker/podman socket
```

The 10-test suite exercises the docker backend's full security envelope
and runs in CI against Docker on GitHub runners. The k8s backend is
verified manually against k3s; it has no CI coverage yet.