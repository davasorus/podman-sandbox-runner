package sandbox

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func newTestPool(t *testing.T) *Pool {
	t.Helper()
	o := baseOpts() // Cmd unused by pools
	o.Cmd = nil
	p, err := NewPool(context.Background(), o)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close(context.Background()) })
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
	p, err := NewPool(context.Background(), o)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { p.Close(context.Background()) })

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
	p.Close(context.Background())
	if _, _, _, err := poolRun(t, p, "echo", "hi"); err == nil {
		t.Error("run on closed pool succeeded; want error")
	}
}
