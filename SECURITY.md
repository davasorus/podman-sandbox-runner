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
  design boundary, not a vulnerability.
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