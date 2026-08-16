package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	o := baseOpts() // Cmd unused by pools
	o.Cmd = nil
	p, err := NewPool(context.Background(), o, 1)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func poolRun(t *testing.T, p *Pool, cmd ...string) (Result, string, string, error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	res, err := p.Run(context.Background(), cmd, nil, &out, &errBuf)
	return res, out.String(), errBuf.String(), err
}

func TestPoolBasicAndReuse(t *testing.T) {
	p := newTestPool(t)

	res, stdout, _, err := poolRun(t, p, "echo", "hello")
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(stdout) != "hello" {
		t.Fatalf("run 1: exit=%d stdout=%q", res.ExitCode, stdout)
	}

	// second run reuses the warm holder and should be fast
	res, stdout, _, err = poolRun(t, p, "echo", "again")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if strings.TrimSpace(stdout) != "again" {
		t.Fatalf("run 2: stdout=%q", stdout)
	}
	if res.DurationMS > 2000 {
		t.Errorf("warm run took %dms; expected well under cold-start time", res.DurationMS)
	}
}

func TestPoolStreamSeparationAndExitCode(t *testing.T) {
	p := newTestPool(t)
	res, stdout, stderr, err := poolRun(t, p, "sh", "-c", "echo out; echo err >&2; exit 3")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(stdout) != "out" || strings.TrimSpace(stderr) != "err" {
		t.Errorf("streams = %q / %q", stdout, stderr)
	}
}

func TestPoolRunsAsWorkloadUser(t *testing.T) {
	p := newTestPool(t)
	_, stdout, _, err := poolRun(t, p, "id", "-u")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(stdout) != "65534" {
		t.Errorf("uid = %q, want 65534", stdout)
	}
}

func TestPoolScratchWipedBetweenRuns(t *testing.T) {
	p := newTestPool(t)

	if _, _, _, err := poolRun(t, p, "sh", "-c", "echo secret > /work/leftover"); err != nil {
		t.Fatalf("write run: %v", err)
	}

	res, stdout, _, err := poolRun(t, p, "sh", "-c", "ls -A /work | wc -l")
	if err != nil {
		t.Fatalf("check run: %v", err)
	}
	if strings.TrimSpace(stdout) != "0" {
		t.Errorf("/work not wiped between runs: %d entries (exit=%d)", len(stdout), res.ExitCode)
	}
}

func TestPoolBackgroundProcessReaped(t *testing.T) {
	p := newTestPool(t)

	// leave a background process behind
	if _, _, _, err := poolRun(t, p, "sh", "-c", "sleep 300 & echo spawned"); err != nil {
		t.Fatalf("spawn run: %v", err)
	}

	// next run should see no surviving sleep owned by the workload user
	_, stdout, _, err := poolRun(t, p, "sh", "-c", "pgrep -U 65534 sleep | wc -l")
	if err != nil {
		t.Fatalf("check run: %v", err)
	}
	// pgrep may count this run's own sh ancestry oddly on some images;
	// assert no long sleep survived rather than exact zero processes
	if strings.TrimSpace(stdout) != "0" {
		t.Errorf("background sleep survived cleanup: pgrep count = %q", stdout)
	}
}

func TestPoolTimeoutLeavesPoolUsable(t *testing.T) {
	o := baseOpts()
	o.Cmd = nil
	o.Timeout = 3 * time.Second
	p, err := NewPool(context.Background(), o, 1)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	res, _, _, err := poolRun(t, p, "sleep", "60")
	if err == nil || !res.TimedOut {
		t.Fatalf("want timeout, got res=%+v err=%v", res, err)
	}

	// pool must recover for the next run
	res, stdout, _, err := poolRun(t, p, "echo", "recovered")
	if err != nil {
		t.Fatalf("post-timeout run: %v", err)
	}
	if strings.TrimSpace(stdout) != "recovered" {
		t.Errorf("post-timeout stdout = %q", stdout)
	}
	_ = res
}

func TestPoolClosedRejectsRuns(t *testing.T) {
	p := newTestPool(t)
	_ = p.Close(context.Background())
	if _, _, _, err := poolRun(t, p, "echo", "hi"); err == nil {
		t.Error("run on closed pool succeeded; want error")
	}
}

func newTestK8sPool(t *testing.T) *Pool {
	t.Helper()
	if os.Getenv("SANDBOX_K8S_TEST") != "1" {
		t.Skip("SANDBOX_K8S_TEST != 1; skipping k8s pool test")
	}
	o := Opts{
		Backend: "k8s",
		Image:   testImage,
		Timeout: 60 * time.Second,
		MemMB:   256,
		CPUs:    1.0,
		User:    "65534:65534",
	}
	p, err := NewPool(context.Background(), o, 1)
	if err != nil {
		t.Fatalf("NewPool(k8s): %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func TestK8sPoolBasicAndReuse(t *testing.T) {
	p := newTestK8sPool(t)

	_, stdout, _, err := poolRun(t, p, "echo", "hello")
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Fatalf("run 1 stdout = %q", stdout)
	}

	res, stdout, _, err := poolRun(t, p, "echo", "again")
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if strings.TrimSpace(stdout) != "again" {
		t.Fatalf("run 2 stdout = %q", stdout)
	}
	// warm exec into a running pod should be quick (no pod creation)
	if res.DurationMS > 5000 {
		t.Errorf("warm k8s run took %dms; expected well under pod-create time", res.DurationMS)
	}
}

func TestK8sPoolStreamSeparationAndExitCode(t *testing.T) {
	p := newTestK8sPool(t)
	res, stdout, stderr, err := poolRun(t, p, "sh", "-c", "echo out; echo err >&2; exit 3")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if strings.TrimSpace(stdout) != "out" || strings.TrimSpace(stderr) != "err" {
		t.Errorf("streams = %q / %q", stdout, stderr)
	}
}

func TestK8sPoolScratchWipedBetweenRuns(t *testing.T) {
	p := newTestK8sPool(t)
	if _, _, _, err := poolRun(t, p, "sh", "-c", "echo secret > /work/leftover"); err != nil {
		t.Fatalf("write run: %v", err)
	}
	_, stdout, _, err := poolRun(t, p, "sh", "-c", "ls -A /work | wc -l")
	if err != nil {
		t.Fatalf("check run: %v", err)
	}
	if strings.TrimSpace(stdout) != "0" {
		t.Errorf("/work not wiped between runs: %q", stdout)
	}
}

func TestK8sPoolBackgroundProcessReaped(t *testing.T) {
	p := newTestK8sPool(t)
	if _, _, _, err := poolRun(t, p, "sh", "-c", "sleep 300 & echo spawned"); err != nil {
		t.Fatalf("spawn run: %v", err)
	}
	_, stdout, _, err := poolRun(t, p, "sh", "-c", "pgrep sleep | wc -l")
	if err != nil {
		t.Fatalf("check run: %v", err)
	}
	// enumeration-based reap: the backgrounded sleep must be gone.
	// (the holder's own PID 1 is "sleep infinity" but pgrep in the
	// workload's mount/pid namespace sees pod PIDs; the 300s sleep is
	// what we assert against — expect it reaped)
	if strings.TrimSpace(stdout) != "1" {
		t.Errorf("sleep count = %q after reap; want 1 (holder's sleep infinity only)", stdout)
	}
}

func TestPoolConcurrentThroughput(t *testing.T) {
	o := baseOpts()
	o.Cmd = nil
	o.Timeout = 30 * time.Second
	const size = 3
	p, err := NewPool(context.Background(), o, size)
	if err != nil {
		t.Fatalf("NewPool(size=%d): %v", size, err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	// Launch `size` runs that each sleep ~2s. If they run concurrently
	// across the 3 holders, wall time is ~2s; if serialized, ~6s. Assert
	// well under the serialized time to prove overlap.
	const n = size
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out, errBuf bytes.Buffer
			res, err := p.Run(context.Background(),
				[]string{"sh", "-c", "sleep 2; echo done"},
				nil, &out, &errBuf)
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}
			if res.ExitCode != 0 || strings.TrimSpace(out.String()) != "done" {
				errs <- fmt.Errorf("run %d: exit=%d out=%q", i, res.ExitCode, out.String())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("3 concurrent 2s-runs took %v; expected ~2s (overlap), not ~6s (serialized)", elapsed)
	}
	t.Logf("3 concurrent runs completed in %v", elapsed)
}

func TestPoolConcurrentIsolation(t *testing.T) {
	o := baseOpts()
	o.Cmd = nil
	const size = 2
	p, err := NewPool(context.Background(), o, size)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })

	// Each of many runs writes a marker to /work then checks nothing else
	// is there — proving scratch is clean per-run even under concurrent
	// dispatch across holders. Run more than `size` so holders are reused.
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out, errBuf bytes.Buffer
			// count existing /work entries before writing our own
			res, err := p.Run(context.Background(),
				[]string{"sh", "-c", "n=$(ls -A /work | wc -l); echo mine > /work/marker; echo $n"},
				nil, &out, &errBuf)
			if err != nil {
				errs <- fmt.Errorf("run %d: %w", i, err)
				return
			}
			if strings.TrimSpace(out.String()) != "0" {
				errs <- fmt.Errorf("run %d saw %q pre-existing /work entries; scratch not clean", i, strings.TrimSpace(out.String()))
			}
			_ = res
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
