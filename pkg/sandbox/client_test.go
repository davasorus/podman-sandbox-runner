package sandbox

import (
	"context"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmitHTTP(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	rr, err := Submit(context.Background(), ts.URL, RunRequest{Cmd: []string{"echo", "http-submit"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rr.Error != "" {
		t.Fatalf("run error: %s", rr.Error)
	}
	if rr.ExitCode != 0 || strings.TrimSpace(string(rr.Stdout)) != "http-submit" {
		t.Errorf("exit=%d stdout=%q", rr.ExitCode, rr.Stdout)
	}
}

func TestSubmitUnix(t *testing.T) {
	o := baseOpts()
	o.Cmd = nil
	p, err := NewPool(context.Background(), o, PoolConfig{Min: 1, Max: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	srv := NewServer(p)

	sock := filepath.Join(t.TempDir(), "sandbox.sock")
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeUnix(l) }()
	t.Cleanup(func() { _ = l.Close() })

	rr, err := Submit(context.Background(), "unix://"+sock, RunRequest{Cmd: []string{"echo", "unix-submit"}})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if rr.ExitCode != 0 || strings.TrimSpace(string(rr.Stdout)) != "unix-submit" {
		t.Errorf("exit=%d stdout=%q", rr.ExitCode, rr.Stdout)
	}
}

func TestSubmitRejectsBadScheme(t *testing.T) {
	for _, ep := range []string{"/tmp/sock", "127.0.0.1:8099", "ftp://x", ""} {
		if _, err := Submit(context.Background(), ep, RunRequest{Cmd: []string{"echo", "x"}}); err == nil {
			t.Errorf("endpoint %q should have been rejected", ep)
		}
	}
}
