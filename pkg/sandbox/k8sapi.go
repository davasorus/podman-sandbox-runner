package sandbox

import (
	"bytes"
	"context"
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

	ns := os.Getenv("SANDBOX_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	name := "sandbox-" + rand.String(8)

	uid, gid := int64(65534), int64(65534)
	useUser := o.User != "" // k8s can't express "image default" per-field; empty = don't set

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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"app": "sandbox-runner"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			EnableServiceLinks:           ptr(false),
			Containers: []corev1.Container{{
				Name:            "sandbox",
				Image:           o.Image,
				Command:         o.Cmd,
				Env:             envs,
				WorkingDir:      "/work",
				Stdin:           o.WithStdin,
				StdinOnce:       o.WithStdin,
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

	if o.NetWait > 0 {
		pod.Spec.InitContainers = []corev1.Container{{
			Name:    "netwait",
			Image:   "docker.io/library/busybox:latest",
			Command: []string{"sleep", fmt.Sprintf("%d", int(o.NetWait.Seconds()))},
		}}
	}

	if len(o.Binds) > 0 {
		return res, fmt.Errorf("k8s backend: -v host mounts not supported (host paths are node paths); use -i or bake files into the image")
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
		cs.CoreV1().Pods(ns).Delete(delCtx, created.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace})
	}()

	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	// wait for the pod to leave Pending (attach needs a running container)
	if err := waitPodRunning(runCtx, cs, ns, created.Name); err != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			return res, fmt.Errorf("timed out after %s (pod never started)", o.Timeout)
		}
		return res, err
	}

	// stdin needs a live attach; output comes from the logs API afterward
	// (attach can't connect before start, so fast containers would lose output)
	if o.WithStdin {
		req := cs.CoreV1().RESTClient().Post().
			Resource("pods").Namespace(ns).Name(created.Name).
			SubResource("attach").
			VersionedParams(&corev1.PodAttachOptions{
				Container: "sandbox",
				Stdin:     true,
				Stdout:    true, // must consume streams for stdin to flow
				Stderr:    true,
			}, scheme.ParameterCodec)

		exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
		if err != nil {
			return res, fmt.Errorf("executor: %w", err)
		}
		go exec.StreamWithContext(runCtx, remotecommand.StreamOptions{
			Stdin:  stdin,
			Stdout: io.Discard, // real output comes from logs; avoid duplicates
			Stderr: io.Discard,
		})
	}

	// wait for terminal phase
	exitCode, waitErr := waitPodDone(runCtx, cs, ns, created.Name)
	res.DurationMS = time.Since(started).Milliseconds()

	if waitErr != nil {
		if runCtx.Err() != nil {
			res.TimedOut = true
			return res, fmt.Errorf("timed out after %s (pod killed)", o.Timeout)
		}
		return res, waitErr
	}
	res.ExitCode = exitCode

	// full output via logs API (k8s logs merge stdout+stderr; stderr writer unused here)
	logCtx, logCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer logCancel()
	if rc, err := cs.CoreV1().Pods(ns).GetLogs(created.Name, &corev1.PodLogOptions{Container: "sandbox"}).Stream(logCtx); err == nil {
		io.Copy(stdout, rc)
		rc.Close()
	}

	// OOM detection: k8s surfaces it as the terminated reason
	if p, err := cs.CoreV1().Pods(ns).Get(context.Background(), created.Name, metav1.GetOptions{}); err == nil {
		for _, cst := range p.Status.ContainerStatuses {
			if cst.State.Terminated != nil && cst.State.Terminated.Reason == "OOMKilled" {
				res.OOMKilled = true
			}
		}
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

func waitPodDone(ctx context.Context, cs *kubernetes.Clientset, ns, name string) (int, error) {
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			for _, cst := range p.Status.ContainerStatuses {
				if t := cst.State.Terminated; t != nil {
					return int(t.ExitCode), nil
				}
			}
			if p.Status.Phase == corev1.PodFailed {
				return 1, nil
			}
			return 0, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
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
