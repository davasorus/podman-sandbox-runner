package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	o := baseOpts()
	o.Cmd = nil
	p, err := NewPool(context.Background(), o, PoolConfig{Min: 1, Max: 2})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return NewServer(p)
}

// httpDo issues a request with a context (satisfies the noctx linter).
func httpDo(t *testing.T, method, url string, body []byte) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		r = bytes.NewReader(body)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func TestServerHTTPRun(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(RunRequest{Cmd: []string{"echo", "hello"}})
	resp := httpDo(t, http.MethodPost, ts.URL+"/run", body)
	defer func() { _ = resp.Body.Close() }()

	var rr RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rr.Error != "" {
		t.Fatalf("run error: %s", rr.Error)
	}
	if rr.ExitCode != 0 || strings.TrimSpace(string(rr.Stdout)) != "hello" {
		t.Errorf("exit=%d stdout=%q", rr.ExitCode, rr.Stdout)
	}
}

func TestServerHTTPStdinAndExit(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(RunRequest{
		Cmd:   []string{"sh", "-c", "cat; exit 7"},
		Stdin: []byte("piped-in\n"),
	})
	resp := httpDo(t, http.MethodPost, ts.URL+"/run", body)
	defer func() { _ = resp.Body.Close() }()

	var rr RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rr.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", rr.ExitCode)
	}
	if strings.TrimSpace(string(rr.Stdout)) != "piped-in" {
		t.Errorf("stdout = %q, want piped-in", rr.Stdout)
	}
}

func TestServerHTTPBadMethodAndPath(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	r := httpDo(t, http.MethodGet, ts.URL+"/run", nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /run = %d, want 405", r.StatusCode)
	}

	r = httpDo(t, http.MethodGet, ts.URL+"/nope", nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", r.StatusCode)
	}
}

func TestServerEmptyCommand(t *testing.T) {
	srv := newTestServer(t)
	resp := srv.handleRun(context.Background(), RunRequest{})
	if resp.Error == "" {
		t.Error("empty command should return an error")
	}
}

func TestServerUnixRun(t *testing.T) {
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
		t.Fatalf("listen unix: %v", err)
	}
	go func() { _ = srv.ServeUnix(l) }()
	t.Cleanup(func() { _ = l.Close() })

	// one request per connection
	do := func(req RunRequest) RunResponse {
		t.Helper()
		var d net.Dialer
		conn, err := d.DialContext(context.Background(), "unix", sock)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var rr RunResponse
		if err := json.NewDecoder(conn).Decode(&rr); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return rr
	}

	rr := do(RunRequest{Cmd: []string{"echo", "unix-hello"}})
	if rr.Error != "" {
		t.Fatalf("run error: %s", rr.Error)
	}
	if rr.ExitCode != 0 || strings.TrimSpace(string(rr.Stdout)) != "unix-hello" {
		t.Errorf("exit=%d stdout=%q", rr.ExitCode, rr.Stdout)
	}

	// second connection reuses the warm pool
	rr = do(RunRequest{Cmd: []string{"sh", "-c", "echo e >&2; exit 4"}})
	if rr.ExitCode != 4 || strings.TrimSpace(string(rr.Stderr)) != "e" {
		t.Errorf("second run exit=%d stderr=%q", rr.ExitCode, rr.Stderr)
	}
}
