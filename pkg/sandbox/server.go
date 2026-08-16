package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// RunRequest is one sandbox run submitted to a Server, over any transport.
type RunRequest struct {
	Cmd     []string `json:"cmd"`
	Env     []string `json:"env,omitempty"`
	Stdin   []byte   `json:"stdin,omitempty"`   // base64-encoded by encoding/json
	Timeout string   `json:"timeout,omitempty"` // optional per-run override, e.g. "10s"
}

// RunResponse is the result of a run.
type RunResponse struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	TimedOut  bool   `json:"timed_out"`
	OOMKilled bool   `json:"oom_killed"`
	Error     string `json:"error,omitempty"`
}

// Server dispatches RunRequests to a warm Pool. It is transport-agnostic:
// ServeHTTP exposes it over HTTP, ServeUnix over a unix socket. One Server
// wraps one Pool; the Pool provides warmth and autoscaling across requests.
type Server struct {
	pool *Pool
}

// NewServer wraps a Pool. The caller owns the Pool's lifecycle (Close it
// after the Server stops serving).
func NewServer(p *Pool) *Server { return &Server{pool: p} }

// handleRun is the transport-independent core: request in, response out.
// It never returns an error — failures are carried in RunResponse.Error so
// every transport reports them uniformly.
func (s *Server) handleRun(ctx context.Context, req RunRequest) RunResponse {
	var resp RunResponse
	if len(req.Cmd) == 0 {
		resp.Error = "empty command"
		return resp
	}

	// optional per-run timeout override
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil {
			resp.Error = fmt.Sprintf("invalid timeout %q: %v", req.Timeout, err)
			return resp
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	var out, errBuf bytes.Buffer
	var res Result
	var err error
	if len(req.Stdin) > 0 {
		res, err = s.pool.Run(ctx, req.Cmd, bytes.NewReader(req.Stdin), &out, &errBuf)
	} else {
		res, err = s.pool.Run(ctx, req.Cmd, nil, &out, &errBuf)
	}

	resp.ExitCode = res.ExitCode
	resp.Stdout = out.Bytes()
	resp.Stderr = errBuf.Bytes()
	resp.TimedOut = res.TimedOut
	resp.OOMKilled = res.OOMKilled
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

// ServeHTTP handles POST /run with a JSON RunRequest body, returning a JSON
// RunResponse. Other paths/methods get 404/405.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/run" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, RunResponse{Error: "bad request: " + err.Error()})
		return
	}
	resp := s.handleRun(r.Context(), req)
	writeJSON(w, http.StatusOK, resp)
}

// ServeUnix accepts connections on l, handling one JSON RunRequest per
// connection (read request, write response, close). Blocks until l closes.
func (s *Server) ServeUnix(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err // listener closed on shutdown
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute)) // safety bound

	var req RunRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(RunResponse{Error: "bad request: " + err.Error()})
		return
	}
	resp := s.handleRun(context.Background(), req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
