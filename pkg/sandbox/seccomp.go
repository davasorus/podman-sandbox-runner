package sandbox

import (
	_ "embed"
	"os"
	"path/filepath"
	"sync"
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
	seccompOnce sync.Once
	seccompPath string
	seccompErr  error
)

// seccompProfileFile materializes the embedded profile to a stable path on
// disk and returns it. Both the Docker Engine and podman accept a
// "seccomp=<path>" SecurityOpt; podman's compat API (unlike dockerd) does
// NOT accept inline JSON, so a file path is the portable form. The file is
// written once per process into the user cache dir (falling back to the
// system temp dir) and reused.
func seccompProfileFile() (string, error) {
	seccompOnce.Do(func() {
		dir, err := os.UserCacheDir()
		if err != nil || dir == "" {
			dir = os.TempDir()
		}
		dir = filepath.Join(dir, "podman-sandbox-runner")
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			seccompErr = mkErr
			return
		}
		p := filepath.Join(dir, "seccomp.json")
		if wErr := os.WriteFile(p, []byte(seccompProfileJSON), 0o644); wErr != nil {
			seccompErr = wErr
			return
		}
		seccompPath = p
	})
	return seccompPath, seccompErr
}

// securityOpts returns the Docker/podman SecurityOpt list applied to every
// sandbox container: no privilege escalation plus the embedded seccomp
// profile. If the profile file cannot be written, the seccomp entry is
// omitted rather than failing the run — the container still runs under the
// runtime's own default seccomp profile plus CapDrop:ALL, so this is not
// fail-open to "unconfined".
//
// The k8s backend uses the RuntimeDefault profile (see k8sapi.go); shipping
// this custom profile there would require deploying it to every node, which
// is deployment-specific.
func securityOpts() []string {
	opts := []string{"no-new-privileges"}
	if p, err := seccompProfileFile(); err == nil && p != "" {
		opts = append(opts, "seccomp="+p)
	}
	return opts
}
