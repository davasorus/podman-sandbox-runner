# podman-sandbox-runner

Run untrusted commands and code in ephemeral, locked-down containers via the
podman (or docker) socket. Pipe code in, get output out — nothing escapes.

````sh
echo 'print(21 * 2)' | sandbox run -json -i -image docker.io/library/python:3-alpine -- python3 -
{"exit_code":0,"stdout":"42\n","stderr":"","duration_ms":278,"oom_killed":false,"timed_out":false}
````

## Security envelope

Every run gets:

- **No network** — `NetworkMode: none`; no DNS, no egress
- **Read-only rootfs** — writes only to tmpfs at `/work` (64M) and `/tmp` (16M),
  both die with the container
- **All capabilities dropped**, `no-new-privileges`
- **Memory limit** (default 256M) and pids limit (128) — fork bombs and
  memory hogs get killed
- **Hard timeout** (default 30s) — container is force-removed on expiry
- **Guaranteed cleanup** — container removed on every exit path, including
  ctrl-C

## Usage

````
sandbox run [flags] -- <command...>

  -image string     container image (default docker.io/library/alpine:latest)
  -timeout duration hard execution timeout (default 30s)
  -mem int          memory limit in MB (default 256)
  -i                pipe stdin into the container
  -json             emit structured result instead of streaming
````

Without `-json`, stdout/stderr stream live and the process exit code mirrors
the container's. With `-json`, output is captured and one JSON object is
printed; the container exit code is in the payload and the process exits 0
(125 on sandbox-level errors, with an `error` field set).

### Examples

````sh
# stream a command
sandbox run -- sh -c 'echo hello; echo oops >&2'

# execute piped code in a runtime image
echo 'console.log(6*7)' | sandbox run -i -image docker.io/library/node:alpine -- node -

# machine-consumable result
sandbox run -json -timeout 5s -- sh -c 'exit 3'
````

## Connection

Uses the standard Docker client env vars. Point `DOCKER_HOST` at your socket:

````sh
# rootless podman on Linux
export DOCKER_HOST=unix:///run/user/$UID/podman/podman.sock
````

On Windows with a running podman machine, the client defaults to the
`docker_engine` named pipe — runs land on the machine, not any WSL-distro
podman. Know which backend you're on: `podman ps -a` on each side during a
long-running sandbox tells you.

## Caveats

- **`oom_killed` is best-effort.** The inspect flag only reflects PID 1; a
  child process OOM-killed mid-pipeline can report `false`. Treat
  **exit code 137** as the reliable memory-kill signal.
- Image pulls happen implicitly on first use of an image; pull errors are
  ignored so local-only images work. A typo'd image name therefore surfaces
  as a create error, not a pull error.
- Not a VM. Container isolation is kernel-level; hostile-kernel-exploit
  threat models need stronger isolation (gVisor, Kata, a VM).
````
````

Adjust the fenced-block nesting if your editor complains (the outer ```` fence is just for this chat).

````bash
git add README.md
git commit -m "readme"
git push
````

That's the project complete as a standalone tool. The full sweep of these sessions: podman fundamentals (pods, kube, quadlet), the ops pipeline (CI → GHCR → auto-update), and now the API layer with a reusable security primitive. The one thread deliberately left dangling: wiring `sandbox run -json` into your agent as a tool — but that's agent work, not podman learning, for whenever you're back in that codebase.