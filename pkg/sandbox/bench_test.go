package sandbox

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// requireBench skips a benchmark unless SANDBOX_BENCH=1. Benchmarks hit
// real backends and take minutes, so they never run as part of an
// ordinary `go test`.
func requireBench(b *testing.B) {
	b.Helper()
	if os.Getenv("SANDBOX_BENCH") != "1" {
		b.Skip("SANDBOX_BENCH != 1; skipping benchmark")
	}
}

// benchColdOneShot measures full one-shot cost: create + exec + teardown,
// once per iteration. This is the cold path (sandbox.Run).
func benchColdOneShot(b *testing.B, o Opts) {
	b.Helper()
	requireBench(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var out, errb bytes.Buffer
		if _, err := Run(context.Background(), o, nil, &out, &errb); err != nil {
			b.Fatalf("run: %v", err)
		}
	}
}

// benchWarmPool measures warm exec latency: a holder is created and warmed
// once off the clock, then each iteration execs into it. Holder creation
// is deliberately excluded (that is the point of a warm pool).
func benchWarmPool(b *testing.B, o Opts) {
	b.Helper()
	requireBench(b)
	pool, err := NewPool(context.Background(), o, PoolConfig{Min: 1, Max: 1})
	if err != nil {
		b.Fatalf("NewPool: %v", err)
	}
	defer func() { _ = pool.Close(context.Background()) }()

	// warm the holder once, off the clock
	var w, e bytes.Buffer
	if _, err := pool.Run(context.Background(), []string{"true"}, nil, &w, &e); err != nil {
		b.Fatalf("warm-up run: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		e.Reset()
		if _, err := pool.Run(context.Background(), []string{"true"}, nil, &w, &e); err != nil {
			b.Fatalf("pool run: %v", err)
		}
	}
}

// --- docker backend ---

func BenchmarkColdOneShot_Docker(b *testing.B) {
	o := baseOpts("true")
	o.Backend = "docker"
	benchColdOneShot(b, o)
}

func BenchmarkWarmPool_Docker(b *testing.B) {
	o := baseOpts() // pool supplies per-run commands; Cmd stays nil
	o.Backend = "docker"
	benchWarmPool(b, o)
}

// --- k8s backend ---

func BenchmarkColdOneShot_K8s(b *testing.B) {
	if os.Getenv("SANDBOX_K8S_TEST") != "1" {
		b.Skip("SANDBOX_K8S_TEST != 1; skipping k8s benchmark")
	}
	o := baseOpts("true")
	o.Backend = "k8s"
	benchColdOneShot(b, o)
}

func BenchmarkWarmPool_K8s(b *testing.B) {
	if os.Getenv("SANDBOX_K8S_TEST") != "1" {
		b.Skip("SANDBOX_K8S_TEST != 1; skipping k8s benchmark")
	}
	o := baseOpts()
	o.Backend = "k8s"
	benchWarmPool(b, o)
}

// --- runtime comparison (k8s cold path) ---
// Runtime cost is a cold-start phenomenon: a warm pool amortizes holder
// creation, so the runtime's boot cost is paid once, not per run. These
// measure the cold penalty of stronger isolation.

func BenchmarkColdOneShot_K8s_Gvisor(b *testing.B) {
	if os.Getenv("SANDBOX_K8S_TEST") != "1" || os.Getenv("SANDBOX_GVISOR_TEST") != "1" {
		b.Skip("needs SANDBOX_K8S_TEST=1 and SANDBOX_GVISOR_TEST=1")
	}
	o := baseOpts("true")
	o.Backend = "k8s"
	o.Runtime = "gvisor"
	benchColdOneShot(b, o)
}

func BenchmarkColdOneShot_K8s_Kata(b *testing.B) {
	if os.Getenv("SANDBOX_K8S_TEST") != "1" || os.Getenv("SANDBOX_KATA_TEST") != "1" {
		b.Skip("needs SANDBOX_K8S_TEST=1 and SANDBOX_KATA_TEST=1")
	}
	o := baseOpts("true")
	o.Backend = "k8s"
	o.Runtime = "kata"
	benchColdOneShot(b, o)
}
