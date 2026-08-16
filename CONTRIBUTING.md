# Contributing

PRs welcome. Ground rules:

- CI must pass — that includes both integration suites and the
  govulncheck gate.
- The docker-backend tests need a docker or podman socket:
  `go test -v -timeout 300s ./...`
- The k8s-backend tests need a cluster and are gated:
  `SANDBOX_K8S_TEST=1 go test -v -timeout 600s -run TestK8s ./pkg/sandbox/`
- The gVisor runtime test is additionally gated behind
  `SANDBOX_GVISOR_TEST=1` and requires a `gvisor` RuntimeClass on the
  cluster (runsc + containerd shim installed). CI installs gVisor into
  its k3s cluster and runs it.
- The Kata runtime test is additionally gated behind
  `SANDBOX_KATA_TEST=1` and requires a `kata` RuntimeClass on the
  cluster (kata-static + containerd shim installed) and host
  virtualization (`/dev/kvm`; nested virt enabled under WSL2). CI
  installs Kata into its k3s cluster and runs it; locally, run it only
  where KVM is available.
- Security-relevant changes (anything touching the container/pod
  configuration in `pkg/sandbox/`) should say so in the PR description.
- New dependencies must pass the govulncheck CI gate; exceptions
  require a written rationale in SECURITY.md and `.govulncheck-allow`.