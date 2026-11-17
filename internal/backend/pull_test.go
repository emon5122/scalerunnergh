package backend

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

// fakeDaemon is a tiny image store behind the docker CLI fake: a tag points at
// an image id, images keep their RepoDigests, containers use image ids.
type fakeDaemon struct {
	mu         sync.Mutex
	tags       map[string]string   // image ref -> id
	digests    map[string][]string // id -> RepoDigests
	containers map[string]string   // container -> image id
	remote     map[string]string   // image ref -> id the registry serves
	pullErr    error
	calls      []string
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{tags: map[string]string{}, digests: map[string][]string{}, containers: map[string]string{}, remote: map[string]string{}}
}

func (f *fakeDaemon) dangling() []string {
	tagged := map[string]bool{}
	for _, id := range f.tags {
		tagged[id] = true
	}
	var out []string
	for id := range f.digests {
		if !tagged[id] {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakeDaemon) run(_ context.Context, _ []byte, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case args[0] == "pull":
		if f.pullErr != nil {
			return "", f.pullErr
		}
		ref := args[len(args)-1]
		id := f.remote[ref]
		f.tags[ref] = id
		if _, ok := f.digests[id]; !ok {
			f.digests[id] = []string{repoOf(ref) + "@sha256:d-" + id}
		}
		return ref, nil
	case cmd == "image inspect -f {{.Id}} "+args[len(args)-1]:
		if id, ok := f.tags[args[len(args)-1]]; ok {
			return id, nil
		}
		if _, ok := f.digests[args[len(args)-1]]; ok {
			return args[len(args)-1], nil
		}
		return "", errors.New("Error: No such image: " + args[len(args)-1])
	case strings.HasPrefix(cmd, "image inspect -f {{json .RepoDigests}}"):
		id := args[len(args)-1]
		if t, ok := f.tags[id]; ok {
			id = t
		}
		ds, ok := f.digests[id]
		if !ok {
			return "", errors.New("Error: No such image: " + id)
		}
		if len(ds) == 0 {
			return "[]", nil
		}
		return `["` + strings.Join(ds, `","`) + `"]`, nil
	case cmd == "images -q --no-trunc --filter dangling=true":
		return strings.Join(f.dangling(), "\n"), nil
	case cmd == "ps -aq --no-trunc":
		var ids []string
		for c := range f.containers {
			ids = append(ids, c)
		}
		return strings.Join(ids, "\n"), nil
	case args[0] == "inspect":
		var out []string
		for _, c := range args[3:] {
			out = append(out, f.containers[c])
		}
		return strings.Join(out, "\n"), nil
	case args[0] == "rmi":
		id := args[1]
		for _, img := range f.containers {
			if img == id {
				return "", errors.New("Error response from daemon: conflict: unable to delete " + id + " - image is being used by running container")
			}
		}
		delete(f.digests, id)
		return id, nil
	case args[0] == "create":
		f.containers[args[2]] = f.tags[args[len(args)-1]]
		return "cid", nil
	}
	return "", nil
}

func (f *fakeDaemon) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

const testImage = "ghcr.io/acme/ci-runner:main"

func dockerWith(f *fakeDaemon) *Docker {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = f.run
	return d
}

func TestPullMissingNeverPulls(t *testing.T) {
	f := newFakeDaemon()
	d := dockerWith(f)
	sp := spec()
	sp.Image = testImage
	if _, err := d.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	if n := f.count("pull"); n != 0 {
		t.Fatalf("pull policy missing must not pull, got %d pulls", n)
	}
}

func TestPullAlwaysPullsBeforeCreate(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "sha256:new"
	d := dockerWith(f)
	sp := spec()
	sp.Image, sp.PullAlways = testImage, true
	if _, err := d.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	pullAt, createAt := -1, -1
	for i, c := range f.calls {
		if c == "pull -q "+testImage && pullAt < 0 {
			pullAt = i
		}
		if strings.HasPrefix(c, "create ") {
			createAt = i
		}
	}
	if pullAt < 0 || createAt < pullAt {
		t.Fatalf("pull must come before create: %q", f.calls)
	}
	if f.tags[testImage] != "sha256:new" {
		t.Fatalf("tag not moved: %v", f.tags)
	}
}

func TestPullAlwaysFailureFallsBackToLocalImage(t *testing.T) {
	f := newFakeDaemon()
	f.tags[testImage] = "sha256:local"
	f.digests["sha256:local"] = []string{"ghcr.io/acme/ci-runner@sha256:d-local"}
	f.pullErr = errors.New("docker pull: exit 1: Get https://ghcr.io/v2/: dial tcp: i/o timeout")
	d := dockerWith(f)
	sp := spec()
	sp.Image, sp.PullAlways = testImage, true
	h, err := d.Start(context.Background(), sp)
	if err != nil {
		t.Fatalf("a failed pull must not block the runner: %v", err)
	}
	if f.containers[h.ID] != "sha256:local" {
		t.Fatalf("runner not created from the local image: %v", f.containers)
	}
	if n := f.count("rmi"); n != 0 {
		t.Fatalf("nothing may be pruned after a failed pull: %q", f.calls)
	}
}

func TestPullSharesConcurrentPulls(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "sha256:new"
	d := dockerWith(f)
	// Hold the image lock so that every caller below queues behind one pull.
	p := d.imagePull(testImage)
	p.mu.Lock()
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.Pull(context.Background(), testImage)
		}()
	}
	time.Sleep(20 * time.Millisecond)
	// A pull that finished after all of them asked (in the future, so a
	// goroutine scheduled late still counts as having asked before it).
	p.done = time.Now().Add(time.Hour)
	p.mu.Unlock()
	wg.Wait()
	if n := f.count("pull"); n != 0 {
		t.Fatalf("callers that waited for a finished pull must share it, got %d pulls", n)
	}
	// A caller that asks after the last pull finished pulls again.
	p.mu.Lock()
	p.done = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if err := d.Pull(context.Background(), testImage); err != nil || f.count("pull") != 1 {
		t.Fatalf("later pull: %v, %d pulls", err, f.count("pull"))
	}
}

func TestPullNewDigestPrunesOnlyUnusedImagesOfTheRepo(t *testing.T) {
	f := newFakeDaemon()
	// v1: an older dangling image of this repo, used by a running job.
	f.digests["sha256:v1"] = []string{"ghcr.io/acme/ci-runner@sha256:d-v1"}
	f.containers["srg-h-s-busy"] = "sha256:v1"
	// v2: the image the tag points at now, not used by any container.
	f.tags[testImage] = "sha256:v2"
	f.digests["sha256:v2"] = []string{"ghcr.io/acme/ci-runner@sha256:d-v2"}
	// v0: an older dangling image of this repo, unused.
	f.digests["sha256:v0"] = []string{"ghcr.io/acme/ci-runner@sha256:d-v0"}
	// Another repository's dangling image, unused.
	f.digests["sha256:other"] = []string{"docker.io/library/postgres@sha256:d-other"}
	// A dangling image without RepoDigests (a local build), unused.
	f.digests["sha256:built"] = nil
	f.remote[testImage] = "sha256:v3"

	d := dockerWith(f)
	if err := d.Pull(context.Background(), testImage); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{
		"sha256:v3": true, "sha256:v1": true, "sha256:other": true, "sha256:built": true,
		"sha256:v2": false, "sha256:v0": false,
	} {
		if _, ok := f.digests[id]; ok != want {
			t.Errorf("%s kept=%v, want kept=%v (calls %q)", id, ok, want, f.calls)
		}
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "rmi") && (strings.Contains(c, "-f") || strings.Contains(c, "v1")) {
			t.Errorf("unsafe rmi: %s", c)
		}
	}

	// Same digest again: nothing new, nothing pruned.
	before := f.count("rmi")
	if err := d.Pull(context.Background(), testImage); err != nil {
		t.Fatal(err)
	}
	if f.count("rmi") != before || f.count("images -q") != 1 {
		t.Fatalf("an unchanged image must not prune: %q", f.calls)
	}
}

func TestPruneSkippedWhenContainersCannotBeListed(t *testing.T) {
	f := newFakeDaemon()
	f.tags[testImage] = "sha256:v1"
	f.digests["sha256:v1"] = []string{"ghcr.io/acme/ci-runner@sha256:d-v1"}
	f.remote[testImage] = "sha256:v2"
	d := dockerWith(f)
	inner := f.run
	d.run = func(ctx context.Context, stdin []byte, args ...string) (string, error) {
		if args[0] == "ps" {
			return "", errors.New("docker ps: daemon busy")
		}
		return inner(ctx, stdin, args...)
	}
	if err := d.Pull(context.Background(), testImage); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.digests["sha256:v1"]; !ok || f.count("rmi") != 0 {
		t.Fatalf("prune must be skipped when container use is unknown: %q", f.calls)
	}
}

func TestPullLoopPullsEveryIntervalAndSurvivesFailures(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "sha256:v1"
	f.pullErr = errors.New("registry down")
	d := dockerWith(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.PullLoop(ctx, testImage, 10*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.count("pull") < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	f.mu.Lock()
	f.pullErr = nil
	f.mu.Unlock()
	for func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.tags[testImage] == "" }() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pull loop did not stop with its context")
	}
	if f.count("pull") < 4 {
		t.Fatalf("pull loop must keep pulling after failures: %q", f.calls)
	}
	if f.tags[testImage] != "sha256:v1" {
		t.Fatal("pull loop never pulled the image after the registry came back")
	}
}

func TestRepoOf(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/acme/ci-runner:main":        "ghcr.io/acme/ci-runner",
		"localhost:5000/img":                 "localhost:5000/img",
		"localhost:5000/img:1":               "localhost:5000/img",
		"ubuntu":                             "ubuntu",
		"ghcr.io/acme/ci-runner@sha256:abcd": "ghcr.io/acme/ci-runner",
		"ghcr.io/acme/r:1.0@sha256:abcd":     "ghcr.io/acme/r",
	} {
		if got := repoOf(in); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", in, got, want)
		}
	}
}
