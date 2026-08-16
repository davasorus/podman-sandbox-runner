# podman-sandbox-runner

Run untrusted commands and code in ephemeral, locked-down containers.
Pipe code in, get output out — nothing escapes. Supports docker, podman,
and Kubernetes backends behind one CLI and one Go library, with optional
gVisor or Kata runtime selection, warm pools, and a pooling daemon.

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
- **Optional gVisor or Kata** — `-runtime` interposes a userspace kernel
  (gVisor) or a lightweight VM with its own kernel (Kata) between the
  workload and the host (see Alternate runtimes)

## Usage

```
sandbox run [flags] -- <command...>
sandbox serve [flags]
sandbox version

  -backend string   auto|docker|k8s (default "auto" = docker API)
  -image string     container image (default docker.io/library/alpine:latest)
  -timeout duration hard execution timeout (default 30s)
  -mem int          memory limit in MB (default 256)
  -cpus float       CPU limit in cores (default 1)
  -user string      uid:gid (default "65534:65534"); empty = image default
  -runtime string   alternate OCI runtime (docker backend, e.g. "runsc",
                    "kata") or RuntimeClass name (k8s backend, e.g. "gvisor",
                    "kata"); empty uses the backend default
  -v path:path      mount host file/dir read-only (repeatable; docker/podman only)
  -env KEY=VAL      environment variable (repeatable)
  -i                pipe stdin into the container
  -netwait duration k8s only: init delay so network policy is enforced
                    before the command runs (e.g. 3s; default 0)
  -json             emit structured result instead of streaming
  -via endpoint     submit to a running daemon instead of executing locally:
                    unix:///path/to.sock or http://host:port
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

# Kata (VM-grade) isolation on kubernetes
sandbox run -backend k8s -runtime kata -- uname -a
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

## Warm pools (library)

For latency-sensitive callers making many runs, `sandbox.Pool` keeps
holders warm and executes each run via the exec API — warm runs complete
in well under a second versus seconds for cold starts. The pool
autoscales: it keeps `Min` holders warm, grows on demand up to `Max`, and
closes idle holders (down to `Min`) after `IdleTimeout`.

```go
pool, err := sandbox.NewPool(ctx, opts, sandbox.PoolConfig{
    Min: 1, Max: 8, IdleTimeout: 5 * time.Minute,
})
defer pool.Close(ctx)
res, err := pool.Run(ctx, []string{"python3", "-"}, stdin, &out, &errBuf)
```

`opts.Cmd` is ignored (each `Run` supplies its own command); `opts.User`
is required (workloads exec as this user); `opts.Timeout` applies per run.
Runs are serialized per holder and dispatched across holders concurrently;
for more concurrency raise `Max`.

**Security tradeoff, stated plainly:** runs sharing one holder share a
container. Between runs the pool kills the workload user's processes and
clears `/work` and `/tmp`, but this hygiene is best-effort — runs on one
holder must belong to the same trust domain. For mutually untrusted
callers, use one-shot `Run` or separate pools. The holder runs an init
(PID 1 reaping); workloads exec as `Opts.User`. If a workload kills its
holder (e.g. a cgroup-wide OOM), that holder is dropped and, if demand
requires, replaced by a fresh one — a dead holder is never reused. On the
k8s backend, between-run process reaping is enumeration-based (holder and
workload share a uid) and could in principle be raced by a fork-storm; the
pids limit bounds that. The docker backend reaps atomically.

## Daemon mode (`sandbox serve`)

Warm pools live only inside one process, so per-invocation CLI runs can't
share them. `sandbox serve` runs a long-lived daemon that holds an
autoscaling pool and executes runs submitted over a unix socket and/or a
loopback HTTP listener — giving the CLI and other clients persistent
warmth.

```sh
# start a daemon: 1 warm holder, up to 4 under load, close idle after 5m
sandbox serve -unix /run/user/$UID/sandbox.sock -min 1 -max 4 -idle 5m

# also expose loopback HTTP (for curl / tooling / the library client)
sandbox serve -unix /run/user/$UID/sandbox.sock -http 127.0.0.1:8099
```

Submit work from the CLI with `run -via`:

```sh
sandbox run -via unix:///run/user/$UID/sandbox.sock -- python3 -c 'print(6*7)'
sandbox run -via http://127.0.0.1:8099 -i -- cat < input.txt
```

Over HTTP directly (`POST /run`, JSON body; `stdout`/`stderr` are
base64-encoded in the JSON response):

```sh
curl -s -X POST http://127.0.0.1:8099/run -d '{"cmd":["echo","hi"]}'
```

From Go, via the library client:

```go
resp, err := sandbox.Submit(ctx, "unix:///run/user/1000/sandbox.sock",
    sandbox.RunRequest{Cmd: []string{"echo", "hi"}})
```

Pool flags: `-min` (warm floor; 0 allows scale-to-zero), `-max` (ceiling),
`-idle` (close idle holders after this; 0 = never). Sandbox flags
(`-image`, `-mem`, `-cpus`, `-user`, `-runtime`, `-backend`, ...) apply to
every run the daemon executes and are fixed for the daemon's lifetime.

**Security:** the daemon executes untrusted commands on behalf of anyone
who can reach its socket or port.

- The **unix socket** is created mode 0600 (owner-only) — that is the
  access control; keep it in a directory only you can reach.
- The **HTTP listener has no authentication**. It binds loopback only;
  `serve` refuses a non-loopback `-http` bind unless `-http-allow-remote`
  is passed. Treat reachability of the HTTP port as equivalent to the
  ability to run arbitrary sandboxed commands. There is no TLS — do not
  expose it beyond localhost.

## Benchmarks

Two ways to measure: `go test -bench` (idiomatic, CI-graphable) and
`sandbox bench` (a live dashboard that probes what's available and
measures only those cells).

```sh
# live dashboard — measures whatever backends/runtimes are present
KUBECONFIG=/etc/rancher/k3s/k3s.yaml sandbox bench -n 5

# idiomatic Go benchmarks (gated so they never run in an ordinary test)
SANDBOX_BENCH=1 go test -run '^$' -bench . -benchtime 10x ./pkg/sandbox/
```

Indicative figures from one machine (Ryzen 9 3950X, WSL2; rootless podman
4.9.3 for the docker backend, k3s for k8s). Single-run, not a contract —
run `sandbox bench` on your own host for numbers that matter for your
setup.

| backend | runtime | cold one-shot | warm pool | warm speedup |
| ------- | ------- | ------------- | --------- | ------------ |
| docker (rootless podman) | default | ~540 ms | ~390 ms | ~1.4x |
| k8s | runc | ~1.25 s | ~75 ms | ~16x |
| k8s | gVisor | ~1.45 s | (amortized) | — |
| k8s | Kata | ~2.6 s | (amortized) | — |

Two things worth reading off this:

- **Warm pooling's payoff depends on the backend.** On k8s it is
  dramatic — cold pod-creation is heavy (~1.25 s) while exec into a warm
  holder is ~75 ms, a ~16x win, which is the whole reason `Pool` exists.
  On rootless podman the win is modest (~1.4x) because podman's per-exec
  overhead (~400 ms; conmon + userns attach) dominates and there is little
  cold-start left to amortize. On real dockerd, where `exec` is tens of
  milliseconds, the pool advantage is far larger than the podman row
  suggests.
- **Stronger isolation costs cold-start, not warm throughput.** runc
  ~1.25 s -> gVisor ~1.45 s (+~15%) -> Kata ~2.6 s (+~2x, it boots a real
  guest kernel). A warm pool pays that boot once per holder and amortizes
  it across runs — so the isolation you pay for up front is nearly free
  per run thereafter. This is the argument for pooling Kata specifically.

## Alternate runtimes (gVisor, Kata)

`-runtime` selects a stronger isolation runtime where the backend
supports it, upgrading the "not a VM" caveat into an option. Two are
verified against k3s:

- **gVisor** (`runsc`) — a userspace kernel (the "sentry") that
  intercepts guest syscalls. Lighter than a VM; the guest reports the
  gVisor sentry kernel.
- **Kata** (`kata`) — a lightweight VM with its own real Linux kernel per
  container. Heaviest isolation short of a full VM; the guest reports a
  distinct guest kernel version, not the host's.

```sh
# gVisor: k8s with a "gvisor" RuntimeClass (runsc + containerd shim)
sandbox run -backend k8s -runtime gvisor -- uname -a
# Linux ... 4.19.0-gvisor ...   <- syscalls served by the gVisor sentry

# Kata: k8s with a "kata" RuntimeClass (kata-static + containerd shim)
sandbox run -backend k8s -runtime kata -- uname -r
# 6.x.y  <- a Kata guest kernel, distinct from the host kernel
```

Behavior and caveats, verified against k3s:

- **Fail-closed everywhere.** On k8s, a nonexistent RuntimeClass is
  rejected at pod admission. On the docker backend, the tool inspects
  the created container and errors if the daemon did not honor the
  requested runtime — because **podman's Docker-compat API silently
  ignores `HostConfig.Runtime`**, and a silent downgrade to runc would
  be a security failure. Podman users who want gVisor/Kata must
  configure it daemon-side; note also that **rootless podman cannot run
  runsc at all** (cgroup delegation), so a rootful daemon is required.
- **gVisor memory limits behave differently.** Exceeding the limit may
  stall the workload rather than OOM-kill it: no exit 137, no
  `oom_killed`, the process hangs. Always set a `-timeout` — under
  gVisor it is the effective resource backstop. (Kata, running a real
  kernel, OOM-kills normally.)
- **Kata requires host virtualization.** It boots a real VM per
  container, so the host needs KVM (`/dev/kvm`) and, in nested setups
  (e.g. WSL2), nested virtualization enabled. `kata-runtime check`
  reports host readiness. Where KVM is unavailable, Kata cannot run.
- NetworkPolicy isolation applies to gVisor and Kata pods the same as
  native ones (verified).
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
- Not a VM by default. Plain container isolation is kernel-level; for
  hostile-kernel-exploit threat models use a stronger boundary via
  `-runtime` (gVisor's userspace kernel, or Kata's real VM kernel).

## Development

```sh
go build -o sandbox ./cmd/sandbox
go test -v -timeout 300s ./...     # docker-backend suite; needs a docker/podman socket
```

The suite covers both backends: the docker-backend tests run against
Docker on GitHub runners, and the k8s-backend tests run against a k3s
cluster installed in CI — including gVisor and Kata, which CI installs
(gVisor: runsc + containerd shim + RuntimeClass; Kata: kata-static +
containerd shim + RuntimeClass) and exercises. The k8s tests are gated
behind `SANDBOX_K8S_TEST=1`; the gVisor runtime test additionally behind
`SANDBOX_GVISOR_TEST=1` and the Kata test behind `SANDBOX_KATA_TEST=1`,
each requiring the corresponding RuntimeClass on whatever cluster runs
them (Kata additionally requires host KVM).
