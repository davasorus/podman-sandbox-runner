package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

const testImage = "docker.io/library/alpine:latest"

func baseOpts(cmd ...string) Opts {
	return Opts{
		Image:    testImage,
		Cmd:      cmd,
		Timeout:  30 * time.Second,
		MemMB:    256,
		CPUs:     1.0,
		User:     "65534:65534",
		JSONMode: true, // buffer output so tests can assert on it
	}
}

func TestBasicExecution(t *testing.T) {
	res, err := run(baseOpts("echo", "hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", res.Stdout)
	}
}

func TestStreamSeparationAndExitCode(t *testing.T) {
	res, err := run(baseOpts("sh", "-c", "echo out; echo err >&2; exit 3"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Errorf("streams = %q / %q, want out / err", res.Stdout, res.Stderr)
	}
}

func TestNetworkDisabled(t *testing.T) {
	res, err := run(baseOpts("sh", "-c", "wget -T2 example.com 2>&1"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("network egress succeeded; sandbox is not sealed. output: %s", res.Stdout)
	}
}

func TestReadonlyRootfs(t *testing.T) {
	res, err := run(baseOpts("sh", "-c", "touch /etc/x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("write to rootfs succeeded; want failure")
	}
}

func TestTmpfsWritable(t *testing.T) {
	res, err := run(baseOpts("sh", "-c", "echo scratch > /work/f && cat /work/f"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "scratch" {
		t.Errorf("tmpfs write/read failed: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestRunsAsNobody(t *testing.T) {
	res, err := run(baseOpts("id", "-u"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "65534" {
		t.Errorf("uid = %q, want 65534", res.Stdout)
	}
}

func TestTimeout(t *testing.T) {
	o := baseOpts("sleep", "30")
	o.Timeout = 2 * time.Second
	res, err := run(o)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !res.TimedOut {
		t.Error("TimedOut flag not set")
	}
	if res.DurationMS > 10000 {
		t.Errorf("duration %dms; kill did not happen promptly", res.DurationMS)
	}
}

func TestMemoryKill(t *testing.T) {
	o := baseOpts("sh", "-c", "head -c 200m /dev/zero | tail")
	o.MemMB = 64
	res, err := run(o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 137 {
		t.Errorf("exit = %d, want 137 (SIGKILL)", res.ExitCode)
	}
}

func TestReadOnlyMount(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/hello.txt"
	if err := os.WriteFile(f, []byte("from host\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// read works
	o := baseOpts("cat", "/work/hello.txt")
	o.Binds = []string{f + ":/work/hello.txt:ro"}
	res, err := run(o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "from host" {
		t.Errorf("mount read failed: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// write fails
	o = baseOpts("sh", "-c", "echo x > /mnt/d/hello.txt")
	o.Binds = []string{dir + ":/mnt/d:ro"}
	res, err = run(o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("write through ro mount succeeded; want failure")
	}
}
