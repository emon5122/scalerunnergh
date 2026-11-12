//go:build integration

// Package integration runs the docker backend against a real Docker daemon.
//
//	go test -tags integration ./tests/integration/ -count=1
package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/backend"
	"github.com/emon5122/scalerunnergh/internal/config"
)

const (
	testImage = "scalerunnergh-it:local"
	testHost  = "srg-it-host"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func setup(t *testing.T) *backend.Docker {
	t.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no Docker daemon")
	}
	docker(t, "build", "-q", "-t", testImage, "testimage")
	d := backend.NewDocker(config.DockerBackendConfig{Binary: "docker"}, testHost)
	t.Cleanup(func() { _ = d.Cleanup(context.Background()) })
	return d
}

func TestDockerBackendRunsOneJobWithLimits(t *testing.T) {
	d := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	h, err := d.Start(ctx, backend.RunnerSpec{
		Name: "srg-it-limits", ScaleSet: "it", JITConfig: "JIT-INTEGRATION", Image: testImage,
		Limits: config.Limits{CPUs: 1.5, MemoryMB: 256, Pids: 128, CPUShares: 128, OOMScoreAdj: 1000},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := docker(t, "inspect", "-f",
		"{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.PidsLimit}} {{.HostConfig.CpuShares}} {{.HostConfig.OomScoreAdj}} {{.HostConfig.Privileged}} {{index .Config.Labels \"scalerunner.host\"}}",
		h.ID)
	if want := "1500000000 268435456 268435456 128 128 1000 false " + testHost; got != want {
		t.Fatalf("limits not applied:\n got %s\nwant %s", got, want)
	}
	inspect := docker(t, "inspect", h.ID)
	if strings.Contains(inspect, "JIT-INTEGRATION") || strings.Contains(inspect, "docker.sock") {
		t.Fatal("JIT config or the host docker socket is visible in docker inspect")
	}
	code, err := d.Wait(ctx, h)
	if err != nil || code != 7 {
		t.Fatalf("runner contract broken: exit %d err %v", code, err)
	}
	if err := d.Stop(ctx, h); err != nil {
		t.Fatal(err)
	}
	if out := docker(t, "ps", "-aq", "--filter", "name=srg-it-limits"); out != "" {
		t.Fatalf("container survived Stop: %s", out)
	}
	if err := d.Stop(ctx, h); err != nil {
		t.Fatalf("second Stop must be a no-op: %v", err)
	}
}

func TestDockerBackendStopAndCleanup(t *testing.T) {
	d := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// Two long-running "jobs": one stopped by Stop, one left behind as if the
	// orchestrator had crashed, then removed by Cleanup.
	hold := backend.NewDocker(config.DockerBackendConfig{Binary: "docker", ExtraArgs: []string{"-e", "SRG_HOLD=1", "--cpus", "1", "--memory", "128m"}}, testHost)
	a, err := hold.Start(ctx, backend.RunnerSpec{Name: "srg-it-a", ScaleSet: "it", JITConfig: "JIT-INTEGRATION", Image: testImage})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Start(ctx, backend.RunnerSpec{Name: "srg-it-b", ScaleSet: "it", JITConfig: "JIT-INTEGRATION", Image: testImage}); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { code, _ := hold.Wait(ctx, a); done <- code }()
	time.Sleep(500 * time.Millisecond)
	if err := hold.Stop(ctx, a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Wait did not return after Stop")
	}
	if out := docker(t, "ps", "-q", "--filter", "label=scalerunner.host="+testHost); out == "" {
		t.Fatal("the second runner should still be running")
	}
	if err := d.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if out := docker(t, "ps", "-aq", "--filter", "label=scalerunner.host="+testHost); out != "" {
		t.Fatalf("Cleanup left containers: %s", out)
	}
}
