package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

func TestDockerSeccompBlocksMount(t *testing.T) {
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

func TestSecurityOptsContainsSeccomp(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("no docker/podman client available: %v", err)
	}
	defer func() { _ = cli.Close() }()

	opts := securityOpts(cli)

	var hasNNP, hasSeccomp bool
	for _, o := range opts {
		if o == "no-new-privileges" {
			hasNNP = true
		}
		if strings.HasPrefix(o, "seccomp=") && len(o) > len("seccomp=")+8 {
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
