package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

type k8sPool struct {
	cs   *kubernetes.Clientset
	cfg  *rest.Config
	ns   string
	name string
	opts Opts
}

func newK8sPool(ctx context.Context, o Opts) (*k8sPool, error) {
	if len(o.Binds) > 0 {
		return nil, fmt.Errorf("pool: k8s backend does not support Binds")
	}

	cs, cfg, err := kubeClient()
	if err != nil {
		return nil, err
	}

	ns := os.Getenv("SANDBOX_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	name := "sandbox-pool-" + rand.String(8)

	uid, gid := int64(65534), int64(65534)
	secCtx := &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		RunAsUser:                &uid,
		RunAsGroup:               &gid,
		RunAsNonRoot:             ptr(true),
	}

	memQ := resource.MustParse(fmt.Sprintf("%dMi", o.MemMB))
	cpuQ := resource.MustParse(fmt.Sprintf("%dm", int64(o.CPUs*1000)))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "sandbox-runner"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			ShareProcessNamespace:        ptr(true),
			RuntimeClassName:             runtimeClassOrNil(o.Runtime),
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           o.Image,
				Command:         []string{"sleep", "infinity"},
				WorkingDir:      "/work",
				SecurityContext: secCtx,
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: memQ,
						corev1.ResourceCPU:    cpuQ,
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "work", MountPath: "/work"},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse("64Mi")),
				}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium: corev1.StorageMediumMemory, SizeLimit: ptr(resource.MustParse("16Mi")),
				}}},
			},
		},
	}

	if err := ensureIsolationPolicy(ctx, cs, ns); err != nil {
		return nil, fmt.Errorf("pool: network isolation policy: %w", err)
	}

	created, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("pool: create holder pod: %w", err)
	}

	fail := func(e error) (*k8sPool, error) {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		grace := int64(0)
		_ = cs.CoreV1().Pods(ns).Delete(delCtx, created.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
		return nil, e
	}

	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := waitPodRunning(startCtx, cs, ns, created.Name); err != nil {
		return fail(fmt.Errorf("pool: holder pod never started: %w", err))
	}

	// netwait once for the pod lifetime: NetworkPolicy rules settle here
	// and stay settled for every subsequent run
	if o.NetWait > 0 {
		select {
		case <-time.After(o.NetWait):
		case <-ctx.Done():
			return fail(fmt.Errorf("pool: cancelled during netwait"))
		}
	}

	return &k8sPool{cs: cs, cfg: cfg, ns: ns, name: created.Name, opts: o}, nil
}

func (k *k8sPool) exec(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := k.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(k.ns).Name(k.name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(k.cfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
}

func (k *k8sPool) run(ctx context.Context, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result
	runCtx, cancel := context.WithTimeout(ctx, k.opts.Timeout)
	defer cancel()

	// k8s exec returns only when the stdout/stderr streams close, so a
	// backgrounded child that inherits them hangs the run until timeout
	// (kubernetes/kubernetes#119992). Redirect the workload's stdio to
	// capture files so no child holds the exec streams; replay the
	// captures once the foreground command exits.
	var wrapped []string
	if stdin != nil {
		wrapped = []string{"sh", "-c", `"$@" >/tmp/.o 2>/tmp/.e; ec=$?; cat /tmp/.o; cat /tmp/.e >&2; rm -f /tmp/.o /tmp/.e; exit $ec`, "_"}
	} else {
		wrapped = []string{"sh", "-c", `"$@" >/tmp/.o 2>/tmp/.e </dev/null; ec=$?; cat /tmp/.o; cat /tmp/.e >&2; rm -f /tmp/.o /tmp/.e; exit $ec`, "_"}
	}
	wrapped = append(wrapped, cmd...)

	started := time.Now()
	streamErr := k.exec(runCtx, wrapped, stdin, stdout, stderr)
	res.DurationMS = time.Since(started).Milliseconds()

	if streamErr != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			return res, fmt.Errorf("pool: timed out after %s (workload reaped)", k.opts.Timeout)
		}
		var codeErr utilexec.CodeExitError
		if errors.As(streamErr, &codeErr) {
			res.ExitCode = codeErr.Code
		} else {
			return res, fmt.Errorf("pool: exec: %w", streamErr)
		}
	}
	if res.ExitCode == 137 {
		res.OOMKilled = true
	}
	return res, nil
}

// cleanup reaps by ENUMERATION: k8s exec has no per-exec user, so
// holder and workload share a uid and a kill broadcast would take out
// the holder's PID 1. We kill everything except PID 1 and the cleanup
// shell itself. A hostile fork-storm could theoretically race the
// enumeration; the pod's implicit pids limits and the holder-liveness
// backstop bound the damage. Scratch is cleared afterward.
func (k *k8sPool) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// With shareProcessNamespace, PID 1 is the pause container (a reaping
	// init). Workload-spawned orphans reparent to it (PPID 1); the holder
	// and this cleanup exec are runtime-started (PPID 0). So: SIGKILL every
	// PPID-1 process — that's exactly the orphans — and pause reaps the
	// corpses (no zombies). Holder and cleanup are spared automatically.
	script := `for p in /proc/[0-9]*; do pid=${p#/proc/}; ppid=$(awk '{print $4}' "$p/stat" 2>/dev/null); [ "$ppid" = 1 ] && kill -9 "$pid" 2>/dev/null; done; rm -rf /work/* /tmp/* 2>/dev/null; true`
	_ = k.exec(ctx, []string{"sh", "-c", script}, nil, io.Discard, io.Discard)
}

func (k *k8sPool) checkHolder() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := k.cs.CoreV1().Pods(k.ns).Get(ctx, k.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("holder pod inspect failed: %w", err)
	}
	if p.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("holder pod is %s (possibly killed by a workload)", p.Status.Phase)
	}
	return nil
}

func (k *k8sPool) close(ctx context.Context) error {
	delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	grace := int64(0)
	return k.cs.CoreV1().Pods(k.ns).Delete(delCtx, k.name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
}
