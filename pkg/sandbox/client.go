package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Submit sends a RunRequest to a sandbox daemon and returns its
// RunResponse. endpoint must carry an explicit scheme:
//
//	unix:///run/user/1000/sandbox.sock   -> unix socket
//	http://127.0.0.1:8099                -> HTTP (POST /run)
//
// A missing or unrecognized scheme is rejected (fail-closed: the caller
// must be explicit about which transport, and thus which endpoint, they
// are trusting with untrusted-code execution).
func Submit(ctx context.Context, endpoint string, req RunRequest) (RunResponse, error) {
	switch {
	case strings.HasPrefix(endpoint, "unix://"):
		return submitUnix(ctx, strings.TrimPrefix(endpoint, "unix://"), req)
	case strings.HasPrefix(endpoint, "http://"), strings.HasPrefix(endpoint, "https://"):
		return submitHTTP(ctx, endpoint, req)
	default:
		return RunResponse{}, fmt.Errorf("submit: endpoint %q must start with unix:// or http://", endpoint)
	}
}

func submitHTTP(ctx context.Context, base string, req RunRequest) (RunResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RunResponse{}, fmt.Errorf("submit: marshal: %w", err)
	}
	url := strings.TrimRight(base, "/") + "/run"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RunResponse{}, fmt.Errorf("submit: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return RunResponse{}, fmt.Errorf("submit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var rr RunResponse
	if err := json.NewDecoder(resp.Body).Decode(&rr); err != nil {
		return RunResponse{}, fmt.Errorf("submit: decode: %w", err)
	}
	return rr, nil
}

func submitUnix(ctx context.Context, path string, req RunRequest) (RunResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return RunResponse{}, fmt.Errorf("submit: dial %s: %w", path, err)
	}
	defer func() { _ = conn.Close() }()

	// bound the exchange so a hung daemon can't block the caller forever
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return RunResponse{}, fmt.Errorf("submit: encode: %w", err)
	}
	var rr RunResponse
	if err := json.NewDecoder(conn).Decode(&rr); err != nil {
		return RunResponse{}, fmt.Errorf("submit: decode: %w", err)
	}
	return rr, nil
}
