package sandbox

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const testImage = "docker.io/library/alpine:latest"

func baseOpts(cmd ...string) Opts {
	return Opts{
		Image:   testImage,
		Cmd:     cmd,
		Timeout: 30 * time.Second,
		MemMB:   256,
		CPUs:    1.0,
		User:    "65534:65534",
	}
}

// runTest executes opts against the docker backend, capturing output.
// It fails the test on sandbox-level errors unless the run timed out
// (timeout tests assert on that themselves).
func runTest(t *testing.T, o Opts) (Result, string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	res, err := (&dockerBackend{}).Run(context.Background(), o, nil, &out, &errBuf)
	return res, out.String(), errBuf.String(), err
}

func TestBasicExecution(t *testing.T) {
	res, stdout, _, err := runTest(t, baseOpts("echo", "hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", stdout)
	}
}

func TestStreamSeparationAndExitCode(t *testing.T) {
	res, stdout, stderr, err := runTest(t, baseOpts("sh", "-c", "echo out; echo err >&2; exit 3"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(stdout) != "out" || strings.TrimSpace(stderr) != "err" {
		t.Errorf("streams = %q / %q, want out / err", stdout, stderr)
	}
}

func TestNetworkDisabled(t *testing.T) {
	res, stdout, _, err := runTest(t, baseOpts("sh", "-c", "wget -T2 example.com 2>&1"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("network egress succeeded; sandbox is not sealed. output: %s", stdout)
	}
}

func TestReadonlyRootfs(t *testing.T) {
	res, _, _, err := runTest(t, baseOpts("sh", "-c", "touch /etc/x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("write to rootfs succeeded; want failure")
	}
}

func TestTmpfsWritable(t *testing.T) {
	res, stdout, stderr, err := runTest(t, baseOpts("sh", "-c", "echo scratch > /work/f && cat /work/f"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "scratch" {
		t.Errorf("tmpfs write/read failed: exit=%d stdout=%q stderr=%q", res.ExitCode, stdout, stderr)
	}
}

func TestRunsAsNobody(t *testing.T) {
	_, stdout, _, err := runTest(t, baseOpts("id", "-u"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "65534" {
		t.Errorf("uid = %q, want 65534", stdout)
	}
}

func TestEnvPassthrough(t *testing.T) {
	o := baseOpts("sh", "-c", "echo $GREETING")
	o.Env = []string{"GREETING=hi"}
	_, stdout, _, err := runTest(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Errorf("env = %q, want hi", stdout)
	}
}

func TestTimeout(t *testing.T) {
	o := baseOpts("sleep", "30")
	o.Timeout = 2 * time.Second
	res, _, _, err := runTest(t, o)
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
	res, _, _, err := runTest(t, o)
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
	res, stdout, stderr, err := runTest(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "from host" {
		t.Errorf("mount read failed: exit=%d stdout=%q stderr=%q", res.ExitCode, stdout, stderr)
	}

	// write fails
	o = baseOpts("sh", "-c", "echo x > /mnt/d/hello.txt")
	o.Binds = []string{dir + ":/mnt/d:ro"}
	res, _, _, err = runTest(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("write through ro mount succeeded; want failure")
	}
}
