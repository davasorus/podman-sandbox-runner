package sandbox

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// k8s tests run only when SANDBOX_K8S_TEST=1 (set in CI after k3s install,
// or locally when a cluster is available).
func k8sOpts(t *testing.T, cmd ...string) Opts {
	t.Helper()
	if os.Getenv("SANDBOX_K8S_TEST") != "1" {
		t.Skip("SANDBOX_K8S_TEST != 1; skipping k8s backend test")
	}
	return Opts{
		Backend: "k8s",
		Image:   testImage,
		Cmd:     cmd,
		Timeout: 60 * time.Second, // pods are slower to start than containers
		MemMB:   256,
		CPUs:    1.0,
		User:    "65534:65534",
	}
}

func runK8s(t *testing.T, o Opts) (Result, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	res, err := (&k8sBackend{}).Run(context.Background(), o, nil, &out, &errBuf)
	return res, out.String(), err
}

func TestK8sBasicExecution(t *testing.T) {
	res, stdout, err := runK8s(t, k8sOpts(t, "echo", "hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(stdout) != "hello" {
		t.Errorf("exit=%d stdout=%q, want 0/hello", res.ExitCode, stdout)
	}
}

func TestK8sExitCodeAndStreamSeparation(t *testing.T) {
	o := k8sOpts(t, "sh", "-c", "echo out; echo err >&2; exit 3")
	var outBuf, errBuf bytes.Buffer
	res, err := (&k8sBackend{}).Run(context.Background(), o, nil, &outBuf, &errBuf)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(outBuf.String()) != "out" || strings.TrimSpace(errBuf.String()) != "err" {
		t.Errorf("streams = %q / %q, want out / err (separated)", outBuf.String(), errBuf.String())
	}
}

func TestK8sRunsAsNobody(t *testing.T) {
	_, stdout, err := runK8s(t, k8sOpts(t, "id", "-u"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "65534" {
		t.Errorf("uid = %q, want 65534", stdout)
	}
}

func TestK8sReadonlyRootfs(t *testing.T) {
	res, _, err := runK8s(t, k8sOpts(t, "sh", "-c", "touch /etc/x"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("write to rootfs succeeded; want failure")
	}
}

func TestK8sTmpfsWritable(t *testing.T) {
	res, stdout, err := runK8s(t, k8sOpts(t, "sh", "-c", "echo scratch > /work/f && cat /work/f"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "scratch" {
		t.Errorf("scratch write failed: exit=%d stdout=%q", res.ExitCode, stdout)
	}
}

func TestK8sTimeout(t *testing.T) {
	o := k8sOpts(t, "sleep", "300")
	o.Timeout = 5 * time.Second
	res, _, err := runK8s(t, o)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !res.TimedOut {
		t.Error("TimedOut flag not set")
	}
}

func TestK8sMemoryKill(t *testing.T) {
	o := k8sOpts(t, "sh", "-c", "head -c 200m /dev/zero | tail")
	o.MemMB = 64
	res, _, err := runK8s(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 137 {
		t.Errorf("exit = %d, want 137", res.ExitCode)
	}
	if !res.OOMKilled {
		t.Error("OOMKilled not set (k8s should report this reliably)")
	}
}

func TestK8sNetworkPolicyWithNetwait(t *testing.T) {
	o := k8sOpts(t, "sh", "-c", "wget -T2 -q -O- example.com >/dev/null 2>&1; echo net_exit=$?")
	o.NetWait = 5 * time.Second
	_, stdout, err := runK8s(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout, "net_exit=1") && !strings.Contains(stdout, "net_exit=4") {
		t.Errorf("egress not blocked with netwait: %q", stdout)
	}
}

func TestK8sRejectsBinds(t *testing.T) {
	o := k8sOpts(t, "true")
	o.Binds = []string{"/tmp:/mnt/x:ro"}
	_, _, err := runK8s(t, o)
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("want binds-rejected error, got %v", err)
	}
}
