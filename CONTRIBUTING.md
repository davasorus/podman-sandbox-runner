# Contributing

PRs welcome. Ground rules:

- CI must pass — that includes both integration suites.
- The docker-backend tests need a docker or podman socket:
  `go test -v -timeout 300s ./...`
- The k8s-backend tests need a cluster and are gated:
  `SANDBOX_K8S_TEST=1 go test -v -timeout 600s -run TestK8s ./pkg/sandbox/`
- Security-relevant changes (anything touching the container/pod
  configuration in `pkg/sandbox/`) should say so in the PR description.
- New dependencies must pass the govulncheck CI gate.