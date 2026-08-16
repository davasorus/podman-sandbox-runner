package sandbox

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
)

// seccompProfileJSON is a default-allow seccomp profile that denies the
// syscalls a sandboxed workload never legitimately needs — kernel module
// loading, ptrace, mount/pivot_root, bpf, perf_event_open, keyring calls,
// time-setting, kexec/reboot, swap, and device-node creation. CapDrop:ALL
// and no-new-privileges already neutralize the capability-gated members of
// this set; the profile closes kernel attack surface as defense-in-depth
// and makes the denials explicit rather than implicit.
//
//go:embed seccomp.json
var seccompProfileJSON string

var (
	seccompFileOnce sync.Once
	seccompFilePath string
	seccompFileErr  error
)

// seccompProfileFile materializes the embedded profile to a stable path and
// returns it. Used for the podman backend, whose Docker-compat API treats
// the "seccomp=" SecurityOpt value as a FILE PATH (Docker Engine, by
// contrast, treats it as inline JSON — see securityOpts).
func seccompProfileFile() (string, error) {
	seccompFileOnce.Do(func() {
		dir, err := os.UserCacheDir()
		if err != nil || dir == "" {
			dir = os.TempDir()
		}
		dir = filepath.Join(dir, "podman-sandbox-runner")
		if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
			seccompFileErr = mkErr
			return
		}
		p := filepath.Join(dir, "seccomp.json")
		if wErr := os.WriteFile(p, []byte(seccompProfileJSON), 0o600); wErr != nil {
			seccompFileErr = wErr
			return
		}
		seccompFilePath = p
	})
	return seccompFilePath, seccompFileErr
}

// daemonIsPodman reports whether the connected daemon is podman rather than
// a real Docker Engine. The two accept the seccomp SecurityOpt in mutually
// incompatible forms — Docker Engine expects inline JSON, podman's compat
// API expects a file path — so the value must be formatted per-daemon.
//
// Detection is layered:
//
//  1. Socket-path heuristic (zero-cost, no round-trip): podman's socket
//     path almost always contains "podman" (e.g.
//     unix:///run/user/1000/podman/podman.sock). A positive match is
//     conclusive and returns immediately — the common podman case pays
//     nothing.
//
//  2. Server-version fallback (one round-trip), consulted only when the
//     socket path does NOT positively identify podman — i.e. the ambiguous
//     case of podman reachable behind a non-podman-named socket. It asks
//     the daemon to identify itself and looks for "podman" in the version,
//     platform name, or component names.
//
// If the fallback errors or reports Docker, the daemon is treated as Docker
// (inline JSON). That keeps the common Docker path working even if the
// version call fails, while correctly catching podman-behind-a-generic-
// socket, which the socket heuristic alone would misjudge.
func daemonIsPodman(cli *client.Client) bool {
	if strings.Contains(strings.ToLower(cli.DaemonHost()), "podman") {
		return true
	}
	return serverIsPodman(cli)
}

// serverIsPodman asks the daemon to identify itself. Returns false on any
// error (treat an unreachable/ambiguous daemon as Docker — the safe default
// for the inline-JSON form, which Docker Engine accepts and CI uses).
func serverIsPodman(cli *client.Client) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return false
	}
	if strings.Contains(strings.ToLower(v.Version), "podman") {
		return true
	}
	if strings.Contains(strings.ToLower(v.Platform.Name), "podman") {
		return true
	}
	for _, c := range v.Components {
		if strings.Contains(strings.ToLower(c.Name), "podman") {
			return true
		}
	}
	return false
}

// securityOpts returns the SecurityOpt list applied to every sandbox
// container: no privilege escalation plus the embedded seccomp profile,
// delivered in the form the connected daemon accepts (a file path for
// podman, inline JSON for Docker Engine).
//
// The profile is defense-in-depth atop CapDrop:ALL + no-new-privileges. If
// the podman profile file cannot be written, the seccomp entry is omitted
// rather than failing the run — the container still runs under the daemon's
// own default seccomp profile plus dropped capabilities, so this is not
// fail-open to "unconfined".
//
// The k8s backend uses RuntimeDefault (see k8sapi.go); shipping this custom
// profile there would require deploying it to every node.
func securityOpts(cli *client.Client) []string {
	opts := []string{"no-new-privileges"}
	if daemonIsPodman(cli) {
		if p, err := seccompProfileFile(); err == nil && p != "" {
			opts = append(opts, "seccomp="+p)
		}
		return opts
	}
	// Docker Engine: inline JSON.
	opts = append(opts, "seccomp="+seccompProfileJSON)
	return opts
}
