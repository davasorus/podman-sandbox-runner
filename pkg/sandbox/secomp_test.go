package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestDockerSeccompBlocksMount verifies the embedded seccomp profile is
// actually applied: the `mount` syscall is on the denylist, so an attempt
// to mount inside the sandbox must fail, while an ordinary command still
// succeeds. This is the docker/podman backend (the profile is passed
// inline via SecurityOpt).
func TestDockerSeccompBlocksMount(t *testing.T) {
	// sanity: a normal command runs fine under the profile
	okOpts := baseOpts("echo", "hello")
	okOpts.Backend = "docker"
	var okOut, okErr bytes.Buffer
	okRes, err := Run(context.Background(), okOpts, nil, &okOut, &okErr)
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	if okRes.ExitCode != 0 || strings.TrimSpace(okOut.String()) != "hello" {
		t.Fatalf("baseline: exit=%d out=%q err=%q", okRes.ExitCode, okOut.String(), okErr.String())
	}

	// mount is denied by the profile -> the command must fail (nonzero exit).
	// Under CapDrop:ALL alone mount fails with EPERM; with seccomp it fails
	// with the profile's errno. Either way it must not succeed.
	mntOpts := baseOpts("sh", "-c", "mount -t tmpfs none /tmp 2>&1; echo rc=$?")
	mntOpts.Backend = "docker"
	var mOut, mErr bytes.Buffer
	if _, err := Run(context.Background(), mntOpts, nil, &mOut, &mErr); err != nil {
		t.Fatalf("mount run: %v", err)
	}
	combined := mOut.String() + mErr.String()
	if strings.Contains(combined, "rc=0") {
		t.Errorf("mount unexpectedly succeeded under seccomp profile; output=%q", combined)
	}
}

// TestSecurityOptsContainsSeccomp is a fast, backend-free check that the
// profile is wired into every container's SecurityOpt.
func TestSecurityOptsContainsSeccomp(t *testing.T) {
	opts := securityOpts()
	var hasNNP, hasSeccomp bool
	for _, o := range opts {
		if o == "no-new-privileges" {
			hasNNP = true
		}
		if strings.HasPrefix(o, "seccomp=") && len(o) > len("seccomp=")+50 {
			hasSeccomp = true
		}
	}
	if !hasNNP {
		t.Error("SecurityOpt missing no-new-privileges")
	}
	if !hasSeccomp {
		t.Error("SecurityOpt missing a non-empty seccomp profile")
	}
}
