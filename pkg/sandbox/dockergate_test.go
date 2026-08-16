package sandbox

import (
	"os"
	"testing"
)

// requireDocker gates docker-backend tests behind SANDBOX_DOCKER_TEST=1,
// mirroring the SANDBOX_K8S_TEST gate for the k8s backend. These tests need
// a reachable docker/podman daemon; without the gate they hard-fail (or
// hang) on machines and CI runners that lack one. CI sets SANDBOX_DOCKER_TEST=1
// for the runner that has Docker.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("SANDBOX_DOCKER_TEST != 1; skipping docker-backend test")
	}
}
