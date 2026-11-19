package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

const gb = int64(1_000_000_000)

var gcNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func daysAgo(d float64) time.Time { return gcNow.Add(-time.Duration(d * 24 * float64(time.Hour))) }

func planIDs(p ImagePlan) []string {
	var out []string
	for _, im := range p.Remove {
		out = append(out, im.ID)
	}
	return out
}

func TestPlanImageGCAgeRule(t *testing.T) {
	images := []ImageInfo{
		{ID: "inuse", Tags: []string{"old/thing:1"}, Tagged: daysAgo(30), Size: gb},
		{ID: "scaleset", Tags: []string{"ghcr.io/o/runner:2"}, Tagged: daysAgo(30), Size: gb},
		{ID: "hub", Tags: []string{"ubuntu:22.04"}, Tagged: daysAgo(30), Size: gb},
		{ID: "latest", Tags: []string{"ghcr.io/o/base:latest"}, Tagged: daysAgo(30), Size: gb},
		{ID: "bydigest", Digests: []string{"ghcr.io/o/pinned@sha256:abc"}, Tagged: daysAgo(30), Size: gb},
		{ID: "young", Tags: []string{"tmp/build:1"}, Tagged: gcNow.Add(-time.Hour), Size: gb},
		{ID: "old", Tags: []string{"tmp/old:1"}, Tagged: daysAgo(8), Size: gb},
		{ID: "dangling", Tagged: daysAgo(10), Size: gb},
		{ID: "recent", Tags: []string{"tmp/recent:1"}, Tagged: daysAgo(2), Size: gb},
	}
	keep := []string{"ghcr.io/o/runner:2", "docker.io/library/ubuntu:22.04", "ghcr.io/o/base", "ghcr.io/o/pinned@sha256:abc"}
	p := PlanImageGC(images, map[string]bool{"inuse": true}, keep, ImagePolicy{MaxAge: 7 * 24 * time.Hour, MinAge: 6 * time.Hour}, gcNow)
	if got := planIDs(p); !slices.Equal(got, []string{"dangling", "old"}) {
		t.Fatalf("age rule removes %v, want [dangling old] (oldest first)", got)
	}
	if p.TotalBytes != 9*gb || p.KeptBytes != 7*gb || p.OverCap {
		t.Fatalf("bytes: %+v", p)
	}
}

func TestPlanImageGCCapRule(t *testing.T) {
	images := []ImageInfo{
		{ID: "kept", Tags: []string{"ci:1"}, Tagged: daysAgo(40), Size: 4 * gb},
		{ID: "a", Tags: []string{"x:1"}, Tagged: daysAgo(3), Size: 3 * gb},
		{ID: "b", Tags: []string{"x:2"}, Tagged: daysAgo(2), Size: 3 * gb},
		{ID: "c", Tags: []string{"x:3"}, Tagged: daysAgo(1), Size: 3 * gb},
		{ID: "young", Tags: []string{"x:4"}, Tagged: gcNow.Add(-time.Minute), Size: 3 * gb},
	}
	// 16GB total, cap 10GB: the oldest unprotected go until under the cap; no age rule.
	p := PlanImageGC(images, nil, []string{"ci:1"}, ImagePolicy{MinAge: 6 * time.Hour, MaxBytes: 10 * gb}, gcNow)
	if got := planIDs(p); !slices.Equal(got, []string{"a", "b"}) || p.KeptBytes != 10*gb || p.OverCap {
		t.Fatalf("cap rule: %v %+v", got, p)
	}
	// Cap below what is protected: everything removable goes and the plan says over cap.
	p = PlanImageGC(images, nil, []string{"ci:1"}, ImagePolicy{MinAge: 6 * time.Hour, MaxBytes: 5 * gb}, gcNow)
	if got := planIDs(p); !slices.Equal(got, []string{"a", "b", "c"}) || !p.OverCap || p.KeptBytes != 7*gb {
		t.Fatalf("over cap: %v %+v", got, p)
	}
	// No rules: nothing goes.
	if p := PlanImageGC(images, nil, nil, ImagePolicy{MinAge: time.Hour}, gcNow); len(p.Remove) != 0 {
		t.Fatalf("no rules removed %v", planIDs(p))
	}
}

func TestNormalizeImageRef(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu":                         "ubuntu:latest",
		"docker.io/library/ubuntu:22.04": "ubuntu:22.04",
		"docker.io/emon/img":             "emon/img:latest",
		"localhost:5000/img":             "localhost:5000/img:latest",
		"localhost:5000/img:2":           "localhost:5000/img:2",
		"ghcr.io/o/i@sha256:abc":         "ghcr.io/o/i@sha256:abc",
		"ghcr.io/o/i:1":                  "ghcr.io/o/i:1",
	} {
		if got := NormalizeImageRef(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// gcDaemon is a docker CLI stand-in with images, containers and scripted rmi failures.
type gcDaemon struct {
	images  map[string]ImageInfo // id -> image
	inUse   []string             // image ids containers use
	rmiErr  map[string]string    // arg -> error text
	calls   []string
	unknown bool // builder prune does not know --max-used-space
}

func (f *gcDaemon) run(_ context.Context, _ []byte, args ...string) (string, error) {
	line := strings.Join(args, " ")
	f.calls = append(f.calls, line)
	switch {
	case line == "image ls -q --no-trunc":
		var ids []string
		for id, im := range f.images {
			for range max(1, len(im.Tags)) {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		return strings.Join(ids, "\n"), nil
	case strings.HasPrefix(line, "image inspect -f {\"id\""):
		im, ok := f.images[args[len(args)-1]]
		if !ok {
			return "", errors.New("Error: No such image")
		}
		tags, digests := `null`, `[]`
		if len(im.Tags) > 0 {
			tags = `["` + strings.Join(im.Tags, `","`) + `"]`
		}
		return fmt.Sprintf(`{"id":%q,"tags":%s,"digests":%s,"created":"2020-01-01T00:00:00.123Z","tagged":%q,"size":%d}`,
			im.ID, tags, digests, im.Tagged.Format(time.RFC3339Nano), im.Size), nil
	case strings.HasPrefix(line, "image inspect -f {{.Id}} "):
		ref := args[len(args)-1]
		for id, im := range f.images {
			if id == ref || slices.Contains(im.Tags, ref) {
				return id, nil
			}
		}
		return "", errors.New("Error: No such image: " + ref)
	case line == "ps -aq --no-trunc":
		if len(f.inUse) == 0 {
			return "", nil
		}
		return "c1", nil
	case strings.HasPrefix(line, "inspect -f {{.Image}}"):
		return strings.Join(f.inUse, "\n"), nil
	case args[0] == "rmi":
		if e, ok := f.rmiErr[args[1]]; ok {
			return "", errors.New(e)
		}
		for id, im := range f.images {
			if id == args[1] {
				delete(f.images, id)
				return "", nil
			}
			if i := slices.Index(im.Tags, args[1]); i >= 0 {
				im.Tags = slices.Delete(im.Tags, i, i+1)
				f.images[id] = im
				if len(im.Tags) == 0 {
					delete(f.images, id)
				}
				return "", nil
			}
		}
		return "", errors.New("Error: No such image: " + args[1])
	case args[0] == "builder":
		if f.unknown && slices.Contains(args, "--max-used-space") {
			return "", errors.New("docker builder: exit status 125: unknown flag: --max-used-space")
		}
		return "ID\nTotal:\t2.1GB", nil
	}
	return "", nil
}

func TestDockerGCImages(t *testing.T) {
	f := &gcDaemon{
		images: map[string]ImageInfo{
			"sha256:keep":     {ID: "sha256:keep", Tags: []string{"ghcr.io/o/runner:2"}, Tagged: time.Now().Add(-90 * 24 * time.Hour), Size: gb},
			"sha256:used":     {ID: "sha256:used", Tags: []string{"used:1"}, Tagged: time.Now().Add(-90 * 24 * time.Hour), Size: gb},
			"sha256:one":      {ID: "sha256:one", Tags: []string{"old:1"}, Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
			"sha256:multi":    {ID: "sha256:multi", Tags: []string{"m:1", "m:2"}, Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
			"sha256:dangling": {ID: "sha256:dangling", Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
			"sha256:refused":  {ID: "sha256:refused", Tags: []string{"r:1"}, Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
			"sha256:broken":   {ID: "sha256:broken", Tags: []string{"b:1"}, Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
			"sha256:fresh":    {ID: "sha256:fresh", Tags: []string{"f:1"}, Tagged: time.Now(), Size: gb},
		},
		inUse: []string{"sha256:used"},
		rmiErr: map[string]string{
			"sha256:refused": "Error response from daemon: conflict: unable to remove repository reference (must force) - container x is using its referenced image",
			"sha256:broken":  "Error response from daemon: i/o error",
		},
	}
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = f.run
	pol := ImagePolicy{MaxAge: 7 * 24 * time.Hour, MinAge: 6 * time.Hour}

	dry, err := d.GCImages(context.Background(), []string{"ghcr.io/o/runner:2"}, pol, true)
	if err != nil || len(dry.Planned) != 5 || dry.Removed != 0 || len(f.images) != 8 {
		t.Fatalf("dry run must plan 5 and remove nothing: %+v %v (images %d)", dry, err, len(f.images))
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "rmi") {
			t.Fatalf("dry run removed: %s", c)
		}
	}

	f.calls = nil
	res, err := d.GCImages(context.Background(), []string{"ghcr.io/o/runner:2"}, pol, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 8 || res.Removed != 3 || res.Skipped != 1 || len(res.RemoveError) != 1 || res.FreedBytes != 3*gb {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(res.RemoveError[0], "broken") {
		t.Fatalf("error must name the image: %v", res.RemoveError)
	}
	for _, id := range []string{"sha256:keep", "sha256:used", "sha256:fresh", "sha256:refused", "sha256:broken"} {
		if _, ok := f.images[id]; !ok {
			t.Errorf("%s must stay", id)
		}
	}
	var rmis []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "rmi") {
			rmis = append(rmis, c)
			if strings.Contains(c, "-f") {
				t.Fatalf("forced removal: %s", c)
			}
		}
	}
	// Single-tag and untagged images go by id; the multi-tag one tag by tag after a re-inspect.
	for _, want := range []string{"rmi sha256:one", "rmi sha256:dangling", "rmi m:1", "rmi m:2"} {
		if !slices.Contains(rmis, want) {
			t.Errorf("missing %q in %v", want, rmis)
		}
	}
}

func TestDockerGCImagesTagMovedIsSkipped(t *testing.T) {
	f := &gcDaemon{images: map[string]ImageInfo{
		"sha256:multi": {ID: "sha256:multi", Tags: []string{"m:1", "m:2"}, Tagged: time.Now().Add(-30 * 24 * time.Hour), Size: gb},
	}}
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = func(ctx context.Context, stdin []byte, args ...string) (string, error) {
		if strings.Join(args, " ") == "image inspect -f {{.Id}} m:1" {
			return "sha256:newer", nil // re-pulled since the listing
		}
		return f.run(ctx, stdin, args...)
	}
	res, err := d.GCImages(context.Background(), nil, ImagePolicy{MaxAge: time.Hour}, false)
	if err != nil || res.Removed != 0 || res.Skipped != 1 || len(res.RemoveError) != 0 {
		t.Fatalf("a moved tag must be a skip: %+v %v", res, err)
	}
	if im := f.images["sha256:multi"]; len(im.Tags) != 2 {
		t.Fatalf("no tag may be removed: %+v", im)
	}
}

func TestDockerGCImagesListFailure(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = func(context.Context, []byte, ...string) (string, error) { return "", errors.New("daemon down") }
	if _, err := d.GCImages(context.Background(), nil, ImagePolicy{MaxAge: time.Hour}, false); err == nil {
		t.Fatal("a failed listing must be an error")
	}
}

func TestDockerPruneBuildCache(t *testing.T) {
	f := &gcDaemon{}
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = f.run
	sum, err := d.PruneBuildCache(context.Background(), 10*gb)
	if err != nil || sum != "Total:\t2.1GB" || f.calls[0] != "builder prune -f -a --max-used-space 10000000000" {
		t.Fatalf("prune: %q %v %v", sum, err, f.calls)
	}
	f.calls, f.unknown = nil, true
	if _, err := d.PruneBuildCache(context.Background(), 10*gb); err != nil || len(f.calls) != 2 ||
		f.calls[1] != "builder prune -f -a --keep-storage 10000000000" {
		t.Fatalf("fallback: %v %v", err, f.calls)
	}
}
