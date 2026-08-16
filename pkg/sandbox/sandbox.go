// Package sandbox runs untrusted commands in ephemeral, locked-down
// containers.
//
// Every run gets a hardened configuration: no network, read-only root
// filesystem, memory-backed scratch space, dropped capabilities, a
// non-root user, memory/CPU/pids limits, a hard timeout, and guaranteed
// cleanup. Two backends are provided: the Docker Engine API (which also
// covers podman via its compatibility socket) and Kubernetes.
//
// The isolation boundary is the container runtime and kernel. This
// package defends against sandbox misconfiguration, not kernel
// exploits; threat models that include hostile kernel exploits need a
// stronger boundary (gVisor, Kata Containers, a VM).
package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Opts configures a single sandboxed run.
type Opts struct {
	// Image is the container image reference to run, e.g.
	// "docker.io/library/alpine:latest". If the image is not present
	// locally it is pulled implicitly; pull errors are ignored so that
	// local-only images work, which means a typo'd reference surfaces
	// as a create error rather than a pull error.
	//
	// On the k8s backend the image must contain a "sleep" binary: the
	// pod runs sleep as a holder process and the command executes via
	// the exec API. Scratch-style images without a shell or coreutils
	// will not work there.
	Image string

	// Cmd is the command and arguments to execute, exec-style (no
	// shell interpretation). Wrap in a shell explicitly if needed:
	// []string{"sh", "-c", "..."}.
	Cmd []string

	// Timeout is the hard wall-clock limit for the run. When it
	// expires the container or pod is force-removed and Result.TimedOut
	// is set. It covers execution, not image pull time on the docker
	// backend; on the k8s backend it also covers pod startup.
	Timeout time.Duration

	// NetWait applies to the k8s backend only: a delay between pod
	// start and command execution, giving the cluster's NetworkPolicy
	// controller time to program per-pod rules. Without it, code that
	// attempts network egress in its first moments can race iptables-
	// based enforcement. A few seconds is typically sufficient; zero
	// (the default) applies no delay. Ignored by the docker backend,
	// whose network isolation is structural and race-free.
	NetWait time.Duration

	// MemMB is the memory limit in mebibytes. Processes exceeding it
	// are killed by the kernel; treat Result.ExitCode 137 as the
	// reliable signal (see Result.OOMKilled).
	MemMB int64

	// CPUs is the CPU limit in cores (fractional values allowed, e.g.
	// 0.5). Enforced as a cgroup quota: exceeding it throttles rather
	// than kills.
	CPUs float64

	// User is the uid:gid to run as, e.g. "65534:65534" (nobody). An
	// empty string means the image's default user, which is frequently
	// root — prefer the non-root default unless the workload requires
	// otherwise.
	User string

	// Binds mounts host files or directories into the container in
	// "host-path:container-path" form. Mounts are always read-only:
	// any mode suffix provided is replaced with "ro". Host paths
	// resolve on the machine serving the API — with a remote or
	// VM-backed daemon that is not the local filesystem.
	//
	// The k8s backend rejects Binds entirely: a "host" path there
	// would be a node path, which is rarely intended and dangerous
	// when it is.
	Binds []string

	// Env sets environment variables inside the container, each entry
	// in "KEY=VALUE" form.
	Env []string

	// WithStdin, when true, streams the reader passed to Run into the
	// container's stdin and signals EOF when it is exhausted. This is
	// the mechanism for piping code into an interpreter, e.g.
	// Cmd: []string{"python3", "-"}.
	WithStdin bool

	// Backend selects the execution backend: "docker" targets the
	// Docker Engine API (Docker or podman, located via the standard
	// DOCKER_HOST environment variables); "k8s" targets a Kubernetes
	// cluster (located via KUBECONFIG, ~/.kube/config, or in-cluster
	// config). "auto" and "" select the docker backend.
	Backend string
}

// Result is the outcome of a sandboxed run.
//
// Stdout and Stderr are populated by the CLI when capturing output;
// library callers receive output through the writers passed to Run and
// may leave these empty or fill them from their own buffers.
type Result struct {
	// ExitCode is the command's exit status. 137 indicates the process
	// was SIGKILLed, most commonly by the kernel OOM killer when the
	// memory limit was exceeded.
	ExitCode int `json:"exit_code"`

	// Stdout is the captured standard output, when captured (see type
	// comment).
	Stdout string `json:"stdout"`

	// Stderr is the captured standard error, when captured.
	Stderr string `json:"stderr"`

	// DurationMS is the wall-clock execution time in milliseconds,
	// measured from command start to exit. It excludes image pulls and
	// container/pod creation.
	DurationMS int64 `json:"duration_ms"`

	// OOMKilled is a best-effort indicator that the run was killed for
	// exceeding its memory limit. On the docker backend it reflects
	// only PID 1; on the k8s backend it is inferred from exit code
	// 137. In both cases ExitCode == 137 is the more reliable signal.
	OOMKilled bool `json:"oom_killed"`

	// TimedOut reports that the run was killed because Opts.Timeout
	// expired. When set, the error returned alongside describes the
	// timeout and ExitCode is not meaningful.
	TimedOut bool `json:"timed_out"`

	// Error carries the sandbox-level error message in serialized
	// results (e.g. CLI JSON output). It is set by the caller from the
	// returned error, not by Run itself.
	Error string `json:"error,omitempty"`
}

// Backend executes sandboxed runs. Implementations must enforce the
// isolation properties described in the package documentation and
// guarantee cleanup of created resources on every return path.
//
// stdout and stderr receive the command's output; pass buffers to
// capture it or os.Stdout/os.Stderr to stream it. stdin may be nil when
// Opts.WithStdin is false.
type Backend interface {
	Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error)
}

// New returns the Backend selected by o.Backend. It returns an error
// for unknown backend names.
func New(o Opts) (Backend, error) {
	switch o.Backend {
	case "", "auto", "docker":
		return &dockerBackend{}, nil
	case "k8s":
		return &k8sBackend{}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q", o.Backend)
	}
}

// Run selects a backend via New and executes the run. It is the
// convenience entrypoint for callers that don't need to hold a Backend.
func Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	b, err := New(o)
	if err != nil {
		return Result{}, err
	}
	return b.Run(ctx, o, stdin, stdout, stderr)
}
