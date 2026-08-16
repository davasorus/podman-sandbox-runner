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
  cluster (runsc + containerd shim installed). It is not run in CI;
  run it locally when touching runtime selection.
- Security-relevant changes (anything touching the container/pod
  configuration in `pkg/sandbox/`) should say so in the PR description.
- New dependencies must pass the govulncheck CI gate; exceptions
  require a written rationale in SECURITY.md and `.govulncheck-allow`.