# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via GitHub's private
vulnerability reporting on this repository (Security tab → Report a
vulnerability). Expect an acknowledgment within a week.

## Threat model

This tool executes untrusted code inside containers with a hardened
configuration (no network, read-only rootfs, dropped capabilities,
non-root user, resource limits, timeout). The isolation boundary is the
container runtime and kernel:

- **In scope**: sandbox configuration errors — anything that lets
  sandboxed code reach the network, write outside its scratch space,
  escalate privileges, exceed resource limits, or survive cleanup. This
  includes silent downgrades: the tool fails closed when a requested
  runtime (e.g. gVisor) is not honored by the daemon or cluster. Pool
  mode's between-run hygiene (process reaping, scratch clearing) is
  also in scope; its documented same-trust-domain requirement is a
  design boundary, not a vulnerability. Daemon mode (`sandbox serve`)
  executes commands on behalf of any client that can reach its unix
  socket (mode 0600) or loopback HTTP listener; the HTTP transport is
  unauthenticated by design and loopback-bound (non-loopback binds are
  refused without an explicit override flag). Access to either endpoint
  is equivalent to local sandboxed-command execution and is the
  operator's responsibility to control — it is not a vulnerability.
- **Out of scope**: kernel and container-runtime escape
  vulnerabilities. Container isolation is kernel-level; if your threat
  model includes hostile kernel exploits, use a stronger boundary. The
  optional gVisor runtime (`-runtime`) narrows the kernel-exploit
  exposure by interposing a userspace kernel, but runtime escapes
  remain out of this tool's scope. Also out of scope: the security of
  the daemon/cluster the tool connects to, and images you choose to
  run.

Backend-specific caveats (k8s NetworkPolicy enforcement and its startup
race, `-netwait`, gVisor memory-limit semantics) are documented in the
README and apply to security assessments.

## Seccomp

Every sandboxed container runs under a seccomp profile in addition to
`CapDrop: ALL`, `no-new-privileges`, a read-only rootfs, a non-root user,
and no network:

- **Docker / podman backends** apply a bundled default-allow profile that
  denies 22 syscalls a sandboxed workload never legitimately needs:
  `ptrace`, `mount`, `umount2`, `pivot_root`, `reboot`, `kexec_load`,
  `kexec_file_load`, `init_module`, `finit_module`, `delete_module`,
  `bpf`, `perf_event_open`, `acct`, `swapon`, `swapoff`, `clock_settime`,
  `settimeofday`, `add_key`, `request_key`, `keyctl`, `mknod`, `mknodat`.
  Most are already blocked by dropped capabilities; the profile makes the
  denials explicit and closes kernel attack surface as defense-in-depth.
  Docker Engine and podman accept the seccomp option in different forms
  (inline JSON vs. a file path); the tool detects the daemon and delivers
  the correct form automatically. If the profile cannot be applied it is
  omitted rather than failing the run, so the container falls back to the
  daemon's own default seccomp profile plus dropped capabilities — never
  to unconfined.
- **Kubernetes backend** sets the pod's seccomp profile to
  `RuntimeDefault` (the container runtime's default profile). Shipping the
  custom denylist on k8s would require deploying the profile file to every
  node, which is deployment-specific and out of scope for the tool itself.

## Known accepted vulnerabilities

None currently. Exceptions, when they exist, are tracked in
`.govulncheck-allow` and enforced by CI: govulncheck findings not
listed there fail the build, and each listed entry carries a written
rationale here.

| ID | Component | Assessment |
|----|-----------|------------|
| —  | —         | None       |

## Supported versions

Pre-1.0: only the latest release receives fixes.