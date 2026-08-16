package sandbox

import (
	"context"
	"time"

	"github.com/moby/moby/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProbeResult reports which backends and runtimes are available in the
// current environment. It is used by `sandbox bench` to run only the
// measurable cells, but is exported for any caller that wants to detect
// capabilities before dispatching work.
type ProbeResult struct {
	DockerOK  bool
	DockerErr string

	K8sOK  bool
	K8sErr string

	// Runtimes maps a k8s RuntimeClass name to whether it exists (checked
	// only when K8sOK). Populated for the names in ProbeOpts.Runtimes.
	Runtimes    map[string]bool
	RuntimeErrs map[string]string
}

// ProbeOpts configures a Probe.
type ProbeOpts struct {
	// Runtimes are k8s RuntimeClass names to check for existence, e.g.
	// {"gvisor", "kata"}.
	Runtimes []string
}

// Probe checks backend reachability and k8s RuntimeClass availability. It
// never returns an error; every failure is captured in the result so
// callers get a complete picture in one call.
func Probe(ctx context.Context, p ProbeOpts) ProbeResult {
	res := ProbeResult{
		Runtimes:    make(map[string]bool),
		RuntimeErrs: make(map[string]string),
	}

	// --- docker/podman reachability (same client the backend builds) ---
	func() {
		cli, err := client.New(client.FromEnv)
		if err != nil {
			res.DockerErr = err.Error()
			return
		}
		defer func() { _ = cli.Close() }()
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := cli.Ping(pingCtx, client.PingOptions{}); err != nil {
			res.DockerErr = err.Error()
			return
		}
		res.DockerOK = true
	}()

	// --- k8s reachability (same client the backend builds) ---
	cs, _, err := kubeClient()
	if err != nil {
		res.K8sErr = err.Error()
		return res
	}
	nodeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := cs.CoreV1().Nodes().List(nodeCtx, metav1.ListOptions{Limit: 1}); err != nil {
		res.K8sErr = err.Error()
		return res
	}
	res.K8sOK = true

	// --- runtime class availability (k8s only) ---
	for _, name := range p.Runtimes {
		rcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := cs.NodeV1().RuntimeClasses().Get(rcCtx, name, metav1.GetOptions{})
		cancel()
		if err != nil {
			res.Runtimes[name] = false
			res.RuntimeErrs[name] = err.Error()
			continue
		}
		res.Runtimes[name] = true
	}

	return res
}
