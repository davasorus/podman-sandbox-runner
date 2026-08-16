# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately via GitHub's private vulnerability
reporting on this repository (Security tab → Report a vulnerability).
Expect an acknowledgment within a week.

## Threat model

This tool executes untrusted code inside containers with a hardened
configuration (no network, read-only rootfs, dropped capabilities,
non-root user, resource limits, timeout). The isolation boundary is the
container runtime and kernel:

- **In scope**: sandbox configuration errors — anything that lets sandboxed
  code reach the network, write outside its scratch space, escalate
  privileges, exceed resource limits, or survive cleanup.
- **Out of scope**: kernel and container-runtime escape vulnerabilities.
  Container isolation is kernel-level; if your threat model includes
  hostile kernel exploits, use a stronger boundary (gVisor, Kata, a VM).
  Also out of scope: the security of the daemon/cluster the tool connects
  to, and images you choose to run.

Backend-specific caveats (k8s NetworkPolicy enforcement and its startup
race, `-netwait`) are documented in the README and apply to security
assessments.

## Known accepted vulnerabilities

Tracked in `.govulncheck-allow`, enforced by CI (new findings fail the
build; listed ones are accepted with rationale):

| ID | Component | Assessment |
|----|-----------|------------|
| GO-2026-4887 | github.com/docker/docker | Moby AuthZ plugin bypass. Daemon-side plugin-validation code; this project uses the module only as an API client and does not run the affected code. No fixed release exists (`Fixed in: N/A`). Mitigation applies to the daemon, not this tool. |
| GO-2026-4883 | github.com/docker/docker | Moby plugin privilege off-by-one. Same assessment: daemon-side, client-only usage, no fixed release. |

These entries are removed as soon as a fixed docker/docker release is
available and adopted.

## Supported versions

Pre-1.0: only the latest release receives fixes.