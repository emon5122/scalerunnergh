//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/backend"
)

// buildLabelled builds a tiny image distinct from the base (its own id) and tags it.
func buildLabelled(t *testing.T, tag, label string) string {
	t.Helper()
	cmd := exec.Command("docker", "build", "-q", "-t", tag, "-")
	cmd.Stdin = strings.NewReader("FROM " + testImage + "\nLABEL srg.gc.it=" + label + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build %s: %v: %s", tag, err, out)
	}
	return docker(t, "image", "inspect", "-f", "{{.Id}}", tag)
}

func imageExists(id string) bool {
	return exec.Command("docker", "image", "inspect", id).Run() == nil
}

// TestGCImagesAgainstARealDaemon runs the image gc with an "everything is old" policy. Every image that existed
// before the test is passed as a kept reference, so only the test's own images can go. It runs in CI (or with
// SRG_IT_GC=1) only, never by accident against a developer's daemon.
func TestGCImagesAgainstARealDaemon(t *testing.T) {
	if os.Getenv("CI") != "true" && os.Getenv("SRG_IT_GC") != "1" {
		t.Skip("set SRG_IT_GC=1 to run the gc test against this daemon")
	}
	d := setup(t)
	keep := strings.Fields(docker(t, "image", "ls", "-q", "--no-trunc"))

	one := buildLabelled(t, "srg-gc-it/one:1", "one")
	multi := buildLabelled(t, "srg-gc-it/multi:1", "multi")
	docker(t, "tag", "srg-gc-it/multi:1", "srg-gc-it/multi:2")
	kept := buildLabelled(t, "srg-gc-it/kept:1", "kept")
	used := buildLabelled(t, "srg-gc-it/used:1", "used")
	dangling := buildLabelled(t, "srg-gc-it/dangling:1", "dangling-old")
	newer := buildLabelled(t, "srg-gc-it/dangling:1", "dangling-new") // the old one loses its tag
	docker(t, "create", "--name", "srg-gc-it-stopped", "srg-gc-it/used:1")
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "srg-gc-it-stopped").Run()
		for _, id := range []string{one, multi, kept, used, dangling, newer} {
			_ = exec.Command("docker", "rmi", "-f", id).Run()
		}
	})
	if one == multi || dangling == newer {
		t.Fatal("test images must be distinct")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := d.GCImages(ctx, append(keep, "srg-gc-it/kept:1"), backend.ImagePolicy{MaxAge: time.Nanosecond}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("gc: %+v", res)
	if len(res.RemoveError) > 0 {
		t.Fatalf("removal errors: %v", res.RemoveError)
	}
	for name, id := range map[string]string{"single tag": one, "multi tag": multi, "dangling": dangling, "re-tagged newer": newer} {
		if imageExists(id) {
			t.Errorf("%s image %s must be removed", name, id)
		}
	}
	for name, id := range map[string]string{"kept": kept, "used by a stopped container": used} {
		if !imageExists(id) {
			t.Errorf("%s image %s must stay", name, id)
		}
	}
	for _, id := range keep {
		if !imageExists(id) {
			t.Errorf("pre-existing image %s was removed", id)
		}
	}
}
