package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type k8sBackend struct{}

func kubeClient() (*kubernetes.Clientset, *rest.Config, error) {
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		home, _ := os.UserHomeDir()
		kc = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kc)
	if err != nil {
		// fall back to in-cluster config (running inside a pod)
		if cfg, err = rest.InClusterConfig(); err != nil {
			return nil, nil, fmt.Errorf("kubeconfig: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cs, cfg, nil
}

func (k *k8sBackend) Run(ctx context.Context, o Opts, stdin io.Reader, stdout, stderr io.Writer) (Result, error) {
	var res Result

	cs, cfg, err := kubeClient()
	if err != nil {
		return res, err
	}

	if len(o.Binds) > 0 {
		return res, fmt.Errorf("k8s backend: -v host mounts not supported (host paths are node paths); use -i or bake files into the image")
	}

	ns := os.Getenv("SANDBOX_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	name := "sandbox-" + rand.String(8)

	uid, gid := int64(65534), int64(65534)
	useUser := o.User != ""

	envs := make([]corev1.EnvVar, 0, len(o.Env))
	for _, e := range o.Env {
		if k := bytes.IndexByte([]byte(e), '='); k > 0 {
			envs = append(envs, corev1.EnvVar{Name: e[:k], Value: e[k+1:]})
		}
	}

	secCtx := &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if useUser {
		secCtx.RunAsUser, secCtx.RunAsGroup, secCtx.RunAsNonRoot = &uid, &gid, ptr(true)
	}

	memQ := resource.MustParse(fmt.Sprintf("%dMi", o.MemMB))
	cpuQ := resource.MustParse(fmt.Sprintf("%dm", int64(o.CPUs*1000)))

	// holder keeps the pod alive; the real command runs via exec.
	// +30s so the holder never dies before the timeout logic does.
	holdSecs := int(o.Timeout.Seconds()) + 30

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "sandbox-runner"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			RuntimeClassName:             runtimeClassOrNil(o.Runtime),
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           o.Image,
				Command:         []string{"sleep", fmt.Sprintf("%d", holdSecs)},
				Env:             envs,
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
		return res, fmt.Errorf("network isolation policy: %w", err)
	}

	created, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return res, fmt.Errorf("create pod: %w", err)
	}

	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		grace := int64(0)
		_ = cs.CoreV1().Pods(ns).Delete(delCtx, created.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	}()

	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	if err := waitPodRunning(runCtx, cs, ns, created.Name); err != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			return res, fmt.Errorf("timed out after %s (pod never started)", o.Timeout)
		}
		return res, err
	}

	// netwait: let the CNI program NetworkPolicy rules before the command runs
	if o.NetWait > 0 {
		select {
		case <-time.After(o.NetWait):
		case <-runCtx.Done():
			res.TimedOut = true
			return res, fmt.Errorf("timed out during netwait")
		}
	}

	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(ns).Name(created.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "sandbox",
			Command:   o.Cmd,
			Stdin:     o.WithStdin,
			Stdout:    true,
			Stderr:    true,
			TTY:       false, // no TTY = separated streams
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return res, fmt.Errorf("executor: %w", err)
	}

	started := time.Now()

	streamErr := executor.StreamWithContext(runCtx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})

	res.DurationMS = time.Since(started).Milliseconds()

	if streamErr != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			return res, fmt.Errorf("timed out after %s (pod killed)", o.Timeout)
		}
		var codeErr utilexec.CodeExitError
		if errors.As(streamErr, &codeErr) {
			res.ExitCode = codeErr.Code
		} else {
			return res, fmt.Errorf("exec: %w", streamErr)
		}
	}

	// OOM: exec'd processes killed by the cgroup OOM killer surface as 137;
	// the container status only reports OOMKilled if the holder died, so
	// exit 137 is the signal here (same caveat as docker now).
	if res.ExitCode == 137 {
		res.OOMKilled = true
	}

	return res, nil
}
func waitPodRunning(ctx context.Context, cs *kubernetes.Clientset, ns, name string) error {
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		switch p.Status.Phase {
		case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ensureIsolationPolicy creates (once per namespace) a NetworkPolicy that
// denies all ingress and egress for pods labeled app=sandbox-runner.
// Enforcement depends on the cluster's CNI supporting NetworkPolicy.
func ensureIsolationPolicy(ctx context.Context, cs *kubernetes.Clientset, ns string) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-runner-deny-all"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "sandbox-runner"},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// no Ingress/Egress rules = deny everything
		},
	}
	_, err := cs.NetworkingV1().NetworkPolicies(ns).Create(ctx, np, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func runtimeClassOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
