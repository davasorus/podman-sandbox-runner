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

// TestK8sGvisorRuntime verifies runtime selection end-to-end: it runs
// only when the cluster has a "gvisor" RuntimeClass (in addition to the
// SANDBOX_K8S_TEST gate). The assertion is the gVisor sentry kernel
// appearing in uname output — proof syscalls hit the sentry.
func TestK8sGvisorRuntime(t *testing.T) {
	o := k8sOpts(t, "uname", "-a")
	if os.Getenv("SANDBOX_GVISOR_TEST") != "1" {
		t.Skip("SANDBOX_GVISOR_TEST != 1; skipping gVisor runtime test")
	}
	o.Runtime = "gvisor"
	_, stdout, err := runK8s(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout, "-gvisor") {
		t.Errorf("uname = %q; want gVisor sentry kernel", stdout)
	}
}

// TestK8sKataRuntime verifies VM-grade isolation via the "kata"
// RuntimeClass end-to-end: the pod reports a guest kernel distinct from
// the host, proving it ran inside a Kata VM. Gated behind
// SANDBOX_KATA_TEST=1 and a "kata" RuntimeClass on the cluster.
func TestK8sKataRuntime(t *testing.T) {
	o := k8sOpts(t, "uname", "-r")
	if os.Getenv("SANDBOX_KATA_TEST") != "1" {
		t.Skip("SANDBOX_KATA_TEST != 1; skipping Kata runtime test")
	}
	o.Runtime = "kata"
	_, stdout, err := runK8s(t, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// the Kata guest kernel differs from the WSL2 host kernel; the host
	// kernel string contains "microsoft" / "WSL2", the guest does not
	if strings.Contains(stdout, "microsoft") || strings.Contains(stdout, "WSL2") {
		t.Errorf("kernel %q looks like the host kernel; expected a Kata guest kernel", strings.TrimSpace(stdout))
	}
	if strings.TrimSpace(stdout) == "" {
		t.Errorf("empty uname output")
	}
}

// TestK8sRuntimeFailClosed verifies a nonexistent RuntimeClass is
// rejected at pod admission rather than silently downgraded.
func TestK8sRuntimeFailClosed(t *testing.T) {
	o := k8sOpts(t, "echo", "hi")
	o.Runtime = "nonexistent-runtime-class"
	_, _, err := runK8s(t, o)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want RuntimeClass-not-found error, got %v", err)
	}
}
