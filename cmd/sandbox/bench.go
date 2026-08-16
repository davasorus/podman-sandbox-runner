package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/davasorus/podman-sandbox-runner/pkg/sandbox"
)

var (
	cGreen  = lipgloss.Color("10")
	cYellow = lipgloss.Color("11")
	cRed    = lipgloss.Color("9")
	cGrey   = lipgloss.Color("8")
	cBlue   = lipgloss.Color("12")
	cWhite  = lipgloss.Color("15")
	cTrack  = lipgloss.Color("236")

	stTitle = lipgloss.NewStyle().Bold(true).Foreground(cWhite)
	stDim   = lipgloss.NewStyle().Foreground(cGrey)
	stNum   = lipgloss.NewStyle().Foreground(cWhite)
	stLabel = lipgloss.NewStyle().Foreground(cBlue)
)

type cell struct {
	backend, runtime, mode string
	dur                    time.Duration
	err                    error
	na                     string
}

func runBench(args []string) int {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	n := fs.Int("n", 5, "iterations per measured cell")
	image := fs.String("image", "docker.io/library/alpine:latest", "image to benchmark with")
	_ = fs.Parse(args)
	if *n < 1 {
		*n = 1
	}

	ctx := context.Background()
	runtimes := []string{"gvisor", "kata"}
	probe := sandbox.Probe(ctx, sandbox.ProbeOpts{Runtimes: runtimes})

	var cells []cell
	if probe.DockerOK {
		o := benchOpts(*image, "docker", "")
		cd, ce := measureCold(ctx, o, *n)
		cells = append(cells, cell{"docker", "default", "cold", cd, ce, ""})
		wd, we := measureWarm(ctx, o, *n)
		cells = append(cells, cell{"docker", "default", "warm", wd, we, ""})
	} else {
		cells = append(cells, cell{"docker", "default", "-", 0, nil, reason(probe.DockerErr)})
	}
	if probe.K8sOK {
		o := benchOpts(*image, "k8s", "")
		cd, ce := measureCold(ctx, o, *n)
		cells = append(cells, cell{"k8s", "runc", "cold", cd, ce, ""})
		wd, we := measureWarm(ctx, o, *n)
		cells = append(cells, cell{"k8s", "runc", "warm", wd, we, ""})
		for _, rt := range runtimes {
			if probe.Runtimes[rt] {
				ro := benchOpts(*image, "k8s", rt)
				rd, re := measureCold(ctx, ro, *n)
				cells = append(cells, cell{"k8s", rt, "cold", rd, re, ""})
			} else {
				cells = append(cells, cell{"k8s", rt, "cold", 0, nil, "no runtimeclass"})
			}
		}
	} else {
		cells = append(cells, cell{"k8s", "-", "-", 0, nil, reason(probe.K8sErr)})
	}

	var maxD time.Duration
	for _, c := range cells {
		if c.na == "" && c.err == nil && c.dur > maxD {
			maxD = c.dur
		}
	}

	fmt.Println(renderDashboard(probe, runtimes, *n, cells, maxD))
	return 0
}

func renderDashboard(probe sandbox.ProbeResult, runtimes []string, n int, cells []cell, maxD time.Duration) string {
	find := func(b, rt, m string) (time.Duration, bool) {
		for _, c := range cells {
			if c.backend == b && c.runtime == rt && c.mode == m && c.na == "" && c.err == nil {
				return c.dur, true
			}
		}
		return 0, false
	}

	var b strings.Builder
	b.WriteString(stTitle.Render("sandbox bench") + "\n")
	b.WriteString(stDim.Render("env  ") + envDots(probe, runtimes) + "\n\n")

	if dc, ok := find("docker", "default", "cold"); ok {
		dw, _ := find("docker", "default", "warm")
		b.WriteString(stLabel.Render("docker · default") + "\n")
		fmt.Fprintf(&b, "  cold  %s  %s\n", solidBar(dc, maxD, 20), stNum.Render(fmtDur(dc)))
		fmt.Fprintf(&b, "  warm  %s  %s   %s\n", solidBar(dw, maxD, 20), stNum.Render(fmtDur(dw)), speedup(dc, dw))
		b.WriteString("\n")
	}
	if kc, ok := find("k8s", "runc", "cold"); ok {
		kw, _ := find("k8s", "runc", "warm")
		b.WriteString(stLabel.Render("k8s · runc") + "\n")
		fmt.Fprintf(&b, "  cold  %s  %s\n", solidBar(kc, maxD, 20), stNum.Render(fmtDur(kc)))
		fmt.Fprintf(&b, "  warm  %s  %s   %s\n", solidBar(kw, maxD, 20), stNum.Render(fmtDur(kw)), speedup(kc, kw))
		b.WriteString("\n")
	}

	b.WriteString(stLabel.Render("k8s · isolation runtimes (cold)") + "\n")
	for _, rt := range []string{"runc", "gvisor", "kata"} {
		if d, ok := find("k8s", rt, "cold"); ok {
			fmt.Fprintf(&b, "  %-7s %s  %s\n", rt, solidBar(d, maxD, 20), stNum.Render(fmtDur(d)))
		}
	}

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cGrey).Padding(0, 2)
	return box.Render(strings.TrimRight(b.String(), "\n")) + "\n" +
		stDim.Render(fmt.Sprintf("n=%d · lower is better · warm excludes one-time holder creation", n))
}

func barColor(d, max time.Duration) lipgloss.Color {
	if max == 0 {
		return cGrey
	}
	switch r := float64(d) / float64(max); {
	case r <= 0.34:
		return cGreen
	case r <= 0.67:
		return cYellow
	default:
		return cRed
	}
}

func solidBar(d, max time.Duration, width int) string {
	f := 1
	if max > 0 {
		f = int(float64(d) / float64(max) * float64(width))
	}
	if f > width {
		f = width
	}
	if f < 1 {
		f = 1
	}
	fill := lipgloss.NewStyle().Foreground(barColor(d, max)).Render(strings.Repeat("█", f))
	track := lipgloss.NewStyle().Foreground(cTrack).Render(strings.Repeat("░", width-f))
	return fill + track
}

func speedup(cold, warm time.Duration) string {
	if warm <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(cGreen).Render(fmt.Sprintf("%.1f× warm", float64(cold)/float64(warm)))
}

func envDots(probe sandbox.ProbeResult, runtimes []string) string {
	dot := func(name string, ok bool) string {
		c := cRed
		if ok {
			c = cGreen
		}
		return lipgloss.NewStyle().Foreground(c).Render("●") + " " + name
	}
	parts := []string{dot("docker", probe.DockerOK), dot("k8s", probe.K8sOK)}
	for _, rt := range runtimes {
		parts = append(parts, dot(rt, probe.K8sOK && probe.Runtimes[rt]))
	}
	return strings.Join(parts, "  ")
}

func fmtDur(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func reason(msg string) string {
	if msg == "" {
		return "unavailable"
	}
	const max = 40
	if len(msg) > max {
		return msg[:max-1] + "…"
	}
	return msg
}

func benchOpts(image, backend, runtime string) sandbox.Opts {
	return sandbox.Opts{
		Image:   image,
		Timeout: 30 * time.Second,
		MemMB:   256,
		CPUs:    1.0,
		User:    "65534:65534",
		Backend: backend,
		Runtime: runtime,
	}
}

func measureCold(ctx context.Context, o sandbox.Opts, n int) (time.Duration, error) {
	o.Cmd = []string{"true"}
	start := time.Now()
	for i := 0; i < n; i++ {
		var out, errb bytes.Buffer
		if _, err := sandbox.Run(ctx, o, nil, &out, &errb); err != nil {
			return 0, err
		}
	}
	return time.Since(start) / time.Duration(n), nil
}

func measureWarm(ctx context.Context, o sandbox.Opts, n int) (time.Duration, error) {
	o.Cmd = nil
	pool, err := sandbox.NewPool(ctx, o, sandbox.PoolConfig{Min: 1, Max: 1})
	if err != nil {
		return 0, err
	}
	defer func() { _ = pool.Close(ctx) }()
	var w, e bytes.Buffer
	if _, err := pool.Run(ctx, []string{"true"}, nil, &w, &e); err != nil {
		return 0, err
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		w.Reset()
		e.Reset()
		if _, err := pool.Run(ctx, []string{"true"}, nil, &w, &e); err != nil {
			return 0, err
		}
	}
	return time.Since(start) / time.Duration(n), nil
}
