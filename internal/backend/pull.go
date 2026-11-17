package backend

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// Image pulls for the "always" and "interval:<d>" pull policies. A pull never
// blocks a job: when it fails, the runner starts from the local image (and
// docker create still pulls an image the host does not have at all).

var (
	// alwaysPullTimeout caps a pull in front of a runner start.
	alwaysPullTimeout = 5 * time.Minute
	// backgroundPullTimeout caps an interval pull.
	backgroundPullTimeout = 30 * time.Minute
	// pruneTimeout caps an image prune that runs off the runner start path.
	pruneTimeout = 5 * time.Minute
)

// imagePull serialises the pulls of one image ref. lock is a one-slot
// semaphore rather than a sync.Mutex so that a waiter gives up when its ctx
// ends: an "always" start must not queue for up to backgroundPullTimeout
// behind a slow interval pull of the same image.
type imagePull struct {
	lock chan struct{}
	done time.Time // when the last pull finished (guarded by lock)
	err  error     // and its result (guarded by lock)
}

// acquire takes the image lock, or returns the ctx error when ctx ends first.
func (p *imagePull) acquire(ctx context.Context) error {
	select {
	case p.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *imagePull) release() { <-p.lock }

func (d *Docker) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (d *Docker) imagePull(image string) *imagePull {
	d.pullsMu.Lock()
	defer d.pullsMu.Unlock()
	if d.pulls == nil {
		d.pulls = map[string]*imagePull{}
	}
	p := d.pulls[image]
	if p == nil {
		p = &imagePull{lock: make(chan struct{}, 1)}
		d.pulls[image] = p
	}
	return p
}

// Pull pulls image and, when that brought a new image, prunes the dangling
// images of the same repository that no container uses. Concurrent callers
// share one pull: whoever waited for a pull that finished after it asked
// takes that result instead of pulling again. A caller whose ctx ends while
// it waits for another caller's pull gets the ctx error.
func (d *Docker) Pull(ctx context.Context, image string) error {
	return d.pullShared(ctx, image, false)
}

// pullShared is Pull; with asyncPrune the prune runs in the background
// instead of before it returns.
func (d *Docker) pullShared(ctx context.Context, image string, asyncPrune bool) error {
	asked := time.Now()
	p := d.imagePull(image)
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	if p.done.After(asked) {
		return p.err
	}
	p.err = d.pull(ctx, image, asyncPrune)
	p.done = time.Now()
	return p.err
}

func (d *Docker) pull(ctx context.Context, image string, asyncPrune bool) error {
	before, _ := d.imageID(ctx, image) // "" when the host does not have it yet
	if _, err := d.run(ctx, nil, "pull", "-q", image); err != nil {
		return err
	}
	after, err := d.imageID(ctx, image)
	if err != nil || after == before {
		return nil // nothing new (or cannot tell): nothing to prune
	}
	d.logger().Info("pulled a new image", "image", image, "id", after, "previous", before)
	if before == "" {
		return nil
	}
	if !asyncPrune {
		d.pruneOld(ctx, image, after)
		return nil
	}
	// Off the runner start path, and not bound to the start's ctx.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pruneTimeout)
	d.bg.Go(func() {
		defer cancel()
		d.pruneOld(pctx, image, after)
	})
	return nil
}

func (d *Docker) imageID(ctx context.Context, image string) (string, error) {
	return d.run(ctx, nil, "image", "inspect", "-f", "{{.Id}}", image)
}

// pruneOld removes dangling images of image's repository except keep. It never
// removes an image any container (running or stopped) was created from, and
// `docker rmi` without -f refuses such an image anyway, so a container created
// in between is safe too. Errors only skip the prune.
func (d *Docker) pruneOld(ctx context.Context, image, keep string) {
	log := d.logger().With("image", image)
	repos, err := d.repoDigestRepos(ctx, keep)
	if err != nil || len(repos) == 0 {
		// Without the repo name the old images cannot be told apart from
		// other repositories' dangling images.
		repos = map[string]bool{repoOf(image): true}
	}
	out, err := d.run(ctx, nil, "images", "-q", "--no-trunc", "--filter", "dangling=true")
	if err != nil {
		log.Warn("image prune skipped: cannot list dangling images", "error", err)
		return
	}
	dangling := strings.Fields(out)
	if len(dangling) == 0 {
		return
	}
	inUse, err := d.imagesInUse(ctx)
	if err != nil {
		log.Warn("image prune skipped: cannot list the images containers use", "error", err)
		return
	}
	for _, id := range dangling {
		if id == keep || inUse[id] {
			continue
		}
		own, err := d.repoDigestRepos(ctx, id)
		if err != nil || !overlaps(own, repos) {
			continue // another repository's image (or unknown): not ours to prune
		}
		if _, err := d.run(ctx, nil, "rmi", id); err != nil {
			log.Warn("old image not removed", "id", id, "error", err)
			continue
		}
		log.Info("removed old image", "id", id)
	}
}

// imagesInUse returns the image ids of every container on the daemon.
func (d *Docker) imagesInUse(ctx context.Context) (map[string]bool, error) {
	out, err := d.run(ctx, nil, "ps", "-aq", "--no-trunc")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	used := map[string]bool{}
	if len(ids) == 0 {
		return used, nil
	}
	out, err = d.run(ctx, nil, append([]string{"inspect", "-f", "{{.Image}}"}, ids...)...)
	if err != nil {
		// A container removed between ps and inspect fails the whole call;
		// treat that as "cannot tell" rather than guess.
		return nil, err
	}
	for _, id := range strings.Fields(out) {
		used[id] = true
	}
	return used, nil
}

// repoDigestRepos returns the repositories in an image's RepoDigests
// ("ghcr.io/o/img@sha256:..." gives "ghcr.io/o/img"). Dangling images keep
// their RepoDigests, which is how they are matched to a repository.
func (d *Docker) repoDigestRepos(ctx context.Context, id string) (map[string]bool, error) {
	out, err := d.run(ctx, nil, "image", "inspect", "-f", "{{json .RepoDigests}}", id)
	if err != nil {
		return nil, err
	}
	var digests []string
	if err := json.Unmarshal([]byte(out), &digests); err != nil {
		return nil, err
	}
	repos := map[string]bool{}
	for _, rd := range digests {
		if i := strings.Index(rd, "@"); i > 0 {
			repos[rd[:i]] = true
		}
	}
	return repos, nil
}

// repoOf strips the tag and digest from an image reference.
func repoOf(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image
}

func overlaps(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// pullBeforeStart is the "always" policy: pull, and on failure warn and go on
// with the local image. alwaysPullTimeout also covers the wait for another
// pull of the same image (e.g. a slow interval pull from another scale set),
// so a start never waits longer than that. When the pull brought a new image,
// the old ones are pruned in the background.
func (d *Docker) pullBeforeStart(ctx context.Context, image string) {
	pctx, cancel := context.WithTimeout(ctx, alwaysPullTimeout)
	defer cancel()
	if err := d.pullShared(pctx, image, true); err != nil {
		d.logger().Warn("image pull failed, starting the runner from the local image", "image", image, "error", err)
	}
}

// PullLoop is the "interval:<d>" policy: it pulls image now and then every
// interval until ctx ends. Failures are logged; the next tick tries again.
func (d *Docker) PullLoop(ctx context.Context, image string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, backgroundPullTimeout)
		err := d.Pull(pctx, image)
		cancel()
		if err != nil && ctx.Err() == nil {
			d.logger().Warn("background image pull failed, runners keep the local image", "image", image, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
