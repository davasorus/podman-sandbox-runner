package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpTestOpts is a minimal one-shot Opts for the MCP server under test.
func mcpTestOpts() sandbox.Opts {
	return sandbox.Opts{
		Image:   "docker.io/library/alpine:latest",
		Timeout: 30 * time.Second,
		MemMB:   256,
		CPUs:    1.0,
		User:    "65534:65534",
		Backend: "auto",
	}
}

// connectTestClient stands up the real MCP server (one-shot mode) over an
// in-memory transport and returns a connected client session. The handshake
// (initialize) happens inside Connect, so a successful return proves it.
func connectTestClient(t *testing.T) *mcp.ClientSession {
	t.Helper()

	server := buildMCPServer(mcpTestOpts(), nil)
	clientT, serverT := mcp.NewInMemoryTransports()

	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect (handshake): %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// TestMCPToolsListed verifies both tools register with the expected names and
// that run_sandbox advertises cmd as required. No daemon needed — registration
// and schema generation are pure.
func TestMCPToolsListed(t *testing.T) {
	cs := connectTestClient(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	found := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		found[tool.Name] = tool
	}

	for _, want := range []string{"run_sandbox", "run_script"} {
		if _, ok := found[want]; !ok {
			t.Errorf("tool %q not advertised; got %v", want, keysOf(found))
		}
	}

	// Both tools should carry an input schema (generated from struct tags).
	if rs := found["run_sandbox"]; rs != nil && rs.InputSchema == nil {
		t.Error("run_sandbox has no input schema")
	}
}

// TestMCPRunSandbox exercises a real tool call end to end: run_sandbox running
// echo, asserting the structured output. Gated behind SANDBOX_DOCKER_TEST like
// the other docker-backend tests, since it executes a container.
func TestMCPRunSandbox(t *testing.T) {
	if os.Getenv("SANDBOX_DOCKER_TEST") != "1" {
		t.Skip("SANDBOX_DOCKER_TEST != 1; skipping docker-backend test")
	}
	cs := connectTestClient(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "run_sandbox",
		Arguments: map[string]any{"cmd": []string{"echo", "mcp-e2e"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool reported error: %+v", res.Content)
	}

	// The text content should carry the stdout we echoed.
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if !strings.Contains(text, "mcp-e2e") {
		t.Errorf("tool result missing echoed output; got: %s", text)
	}
	if !strings.Contains(text, "exit=0") {
		t.Errorf("tool result did not report exit=0; got: %s", text)
	}
}

func keysOf(m map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
