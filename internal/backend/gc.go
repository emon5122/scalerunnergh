package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Image garbage collection for the gc config block. The policy (PlanImageGC) is a pure function; GCImages feeds it
// from the daemon and carries the plan out without ever forcing a removal, so docker itself refuses an image a
// container started using after the listing.

// ImageInfo is one image on the daemon.
type ImageInfo struct {
	ID      string
	Tags    []string // RepoTags
	Digests []string // RepoDigests
	// Tagged is when the image got its newest tag on this daemon (Metadata.LastTagTime), else its build time
	// (Created). The build time alone would make a freshly pulled old image look old.
	Tagged time.Time
	Size   int64
}

// ImagePolicy is the image part of the gc config.
type ImagePolicy struct {
	MaxAge   time.Duration // unused images tagged longer ago than this go (0 = no age rule)
	MinAge   time.Duration // images tagged more recently than this always stay
	MaxBytes int64         // then unused images go oldest first until the total is under this (0 = no cap)
}

// ImagePlan is what PlanImageGC decided.
type ImagePlan struct {
	Remove     []ImageInfo
	TotalBytes int64 // every image before removal
	KeptBytes  int64 // what stays (sizes overcount shared layers, so this is an upper bound)
	OverCap    bool  // MaxBytes is set and only protected images are left above it
}

// PlanImageGC decides which images to remove. protectedIDs are images in use by a container or resolved from a kept
// reference; keepRefs are also matched by name against each image's tags and digests.
func PlanImageGC(images []ImageInfo, protectedIDs map[string]bool, keepRefs []string, p ImagePolicy, now time.Time) ImagePlan {
	keep := map[string]bool{}
	for _, r := range keepRefs {
		keep[NormalizeImageRef(r)] = true
	}
	var plan ImagePlan
	var candidates []ImageInfo
	for _, im := range images {
		plan.TotalBytes += im.Size
		if protectedIDs[im.ID] || refKept(im, keep) || now.Sub(im.Tagged) < p.MinAge {
			continue
		}
		candidates = append(candidates, im)
	}
	// Oldest first, so the cap rule removes the oldest; ties by id for a stable plan.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].Tagged.Equal(candidates[j].Tagged) {
			return candidates[i].Tagged.Before(candidates[j].Tagged)
		}
		return candidates[i].ID < candidates[j].ID
	})
	plan.KeptBytes = plan.TotalBytes
	for _, im := range candidates {
		tooOld := p.MaxAge > 0 && now.Sub(im.Tagged) > p.MaxAge
		overCap := p.MaxBytes > 0 && plan.KeptBytes > p.MaxBytes
		if !tooOld && !overCap {
			continue
		}
		plan.Remove = append(plan.Remove, im)
		plan.KeptBytes -= im.Size
	}
	plan.OverCap = p.MaxBytes > 0 && plan.KeptBytes > p.MaxBytes
	return plan
}

func refKept(im ImageInfo, keep map[string]bool) bool {
	for _, t := range im.Tags {
		if keep[NormalizeImageRef(t)] {
			return true
		}
	}
	for _, d := range im.Digests {
		if keep[NormalizeImageRef(d)] {
			return true
		}
	}
	return false
}

// NormalizeImageRef makes a reference comparable with what docker shows in RepoTags/RepoDigests: no tag and no
// digest means ":latest", and Docker Hub's "docker.io/" and "docker.io/library/" prefixes are dropped.
func NormalizeImageRef(ref string) string {
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	if strings.Contains(ref, "@") {
		return ref
	}
	if strings.LastIndex(ref, ":") <= strings.LastIndex(ref, "/") {
		ref += ":latest"
	}
	return ref
}

// ImageGCResult is one GCImages run.
type ImageGCResult struct {
	Count       int      // images on the daemon before the run
	TotalBytes  int64    // their size
	KeptBytes   int64    // the size of what stays after a full run of the plan
	OverCap     bool     // only protected images are left above images_max_bytes
	Planned     []string // short ids the plan removes
	Removed     int      // images actually removed (0 in a dry run)
	FreedBytes  int64    // estimate: the sizes of the removed images
	Skipped     int      // removals docker refused for an expected race (now in use, gone, has children)
	RemoveError []string // removals that failed for another reason
}

// imageInspectFormat prints one image as JSON. LastTagTime is the zero time for images that never got a tag here.
const imageInspectFormat = `{"id":{{json .Id}},"tags":{{json .RepoTags}},"digests":{{json .RepoDigests}},` +
	`"created":{{json .Created}},"tagged":{{json .Metadata.LastTagTime}},"size":{{.Size}}}`

// GCImages applies the image policy to every image on the daemon. keepRefs are the scale sets' images and
// gc.keep_images. The error is set when the run could not decide anything (listing failed); single removals that
// fail are in the result.
func (d *Docker) GCImages(ctx context.Context, keepRefs []string, p ImagePolicy, dryRun bool) (ImageGCResult, error) {
	var res ImageGCResult
	out, err := d.run(ctx, nil, "image", "ls", "-q", "--no-trunc")
	if err != nil {
		return res, fmt.Errorf("list images: %w", err)
	}
	var images []ImageInfo
	seen := map[string]bool{}
	for _, id := range strings.Fields(out) {
		if seen[id] {
			continue // one line per tag
		}
		seen[id] = true
		im, err := d.inspectImage(ctx, id)
		if err != nil {
			if isGoneErr(err) {
				continue // removed since the listing
			}
			return res, fmt.Errorf("inspect image %s: %w", shortID(id), err)
		}
		images = append(images, im)
	}
	// Runners come and go: a container removed between the listing and its inspect fails the call. Try again.
	var protected map[string]bool
	for try := 0; try < 3; try++ {
		if protected, err = d.imagesInUse(ctx); err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return res, fmt.Errorf("list the images containers use: %w", err)
	}
	for _, ref := range keepRefs {
		// By id too: the daemon resolves Docker Hub short names and digests better than string matching.
		if id, err := d.imageID(ctx, ref); err == nil && id != "" {
			protected[id] = true
		}
	}
	plan := PlanImageGC(images, protected, keepRefs, p, time.Now())
	res.Count, res.TotalBytes, res.KeptBytes, res.OverCap = len(images), plan.TotalBytes, plan.KeptBytes, plan.OverCap
	for _, im := range plan.Remove {
		res.Planned = append(res.Planned, shortID(im.ID))
	}
	if dryRun {
		return res, nil
	}
	for _, im := range plan.Remove {
		switch err := d.removeImage(ctx, im); {
		case err == nil:
			res.Removed++
			res.FreedBytes += im.Size
		case isRaceErr(err):
			res.Skipped++
			res.KeptBytes += im.Size
			d.logger().Info("gc: image not removed (in use or changed since the listing)", "id", shortID(im.ID), "reason", err)
		default:
			res.KeptBytes += im.Size
			res.RemoveError = append(res.RemoveError, fmt.Sprintf("remove image %s: %v", shortID(im.ID), err))
		}
	}
	if p.MaxBytes > 0 && res.KeptBytes > p.MaxBytes {
		res.OverCap = true
	}
	return res, nil
}

func (d *Docker) inspectImage(ctx context.Context, id string) (ImageInfo, error) {
	out, err := d.run(ctx, nil, "image", "inspect", "-f", imageInspectFormat, id)
	if err != nil {
		return ImageInfo{}, err
	}
	var raw struct {
		ID      string    `json:"id"`
		Tags    []string  `json:"tags"`
		Digests []string  `json:"digests"`
		Created string    `json:"created"`
		Tagged  time.Time `json:"tagged"`
		Size    int64     `json:"size"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return ImageInfo{}, fmt.Errorf("parse inspect output: %w", err)
	}
	im := ImageInfo{ID: raw.ID, Tags: raw.Tags, Digests: raw.Digests, Tagged: raw.Tagged, Size: raw.Size}
	if created, err := time.Parse(time.RFC3339Nano, raw.Created); err == nil && created.After(im.Tagged) {
		im.Tagged = created // never tagged here (or tagged before it was built: clock skew)
	}
	return im, nil
}

// removeImage never forces. An image with at most one tag is removed by id, which does not depend on where a tag
// points now. A multi-tag image is untagged tag by tag (docker refuses `rmi <id>` for it without -f), each only
// after the tag is proven to still point to this image; removing the last tag deletes it.
func (d *Docker) removeImage(ctx context.Context, im ImageInfo) error {
	if len(im.Tags) <= 1 {
		_, err := d.run(ctx, nil, "rmi", im.ID)
		return err
	}
	for _, tag := range im.Tags {
		id, err := d.imageID(ctx, tag)
		if err != nil {
			if isGoneErr(err) {
				continue // the tag is gone already
			}
			return err
		}
		if id != im.ID {
			return fmt.Errorf("tag %s now points to another image: no such image %s under it", tag, shortID(im.ID))
		}
		if _, err := d.run(ctx, nil, "rmi", tag); err != nil {
			return err
		}
	}
	return nil
}

// PruneBuildCache prunes the daemon's BuildKit cache down to maxBytes. It returns docker's summary line.
func (d *Docker) PruneBuildCache(ctx context.Context, maxBytes int64) (string, error) {
	n := strconv.FormatInt(maxBytes, 10)
	out, err := d.run(ctx, nil, "builder", "prune", "-f", "-a", "--max-used-space", n)
	if err != nil && strings.Contains(err.Error(), "unknown flag") {
		// A docker CLI from before --max-used-space (buildx < 0.17).
		out, err = d.run(ctx, nil, "builder", "prune", "-f", "-a", "--keep-storage", n)
	}
	if err != nil {
		return "", fmt.Errorf("prune build cache: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// RootDir is the daemon's data dir (DockerRootDir) when this machine can see it, else "" (the orchestrator runs in
// a container without the daemon's dir mounted, or talks to a remote daemon).
func (d *Docker) RootDir(ctx context.Context) string {
	out, err := d.run(ctx, nil, "info", "-f", "{{.DockerRootDir}}")
	if err != nil || out == "" {
		return ""
	}
	if fi, err := os.Stat(out); err != nil || !fi.IsDir() {
		return ""
	}
	return out
}

// isGoneErr: the image or tag no longer exists.
func isGoneErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no such image") || strings.Contains(s, "no such object")
}

// isRaceErr: docker refused a removal because the daemon changed since the listing; not a gc failure.
func isRaceErr(err error) bool {
	s := strings.ToLower(err.Error())
	for _, m := range []string{"no such image", "no such object", "conflict", "being used", "dependent child images",
		"now points to another image"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// SweepResult is one native job dir sweep.
type SweepResult struct {
	Removed   []string // job dirs removed
	Leftovers []string // job dirs that could not be removed
}

// SweepJobDirs removes job dirs no live runner owns and no runner is being started into, last modified longer ago
// than maxAge: the safety net under Stop's removal (Windows file locks can make it fail). With dryRun it only lists
// them (as Removed).
func (n *Native) SweepJobDirs(maxAge time.Duration, dryRun bool) (SweepResult, error) {
	var res SweepResult
	entries, err := os.ReadDir(n.jobsDir())
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("read native jobs dir: %w", err)
	}
	for _, e := range entries {
		dir := filepath.Join(n.jobsDir(), e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || time.Since(fi.ModTime()) < maxAge {
			continue // gone, a symlink or a file (never followed), or young
		}
		if !n.claim(dir) {
			continue // a live runner's dir, or one being started or stopped
		}
		if dryRun {
			n.release(dir)
			res.Removed = append(res.Removed, dir)
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, ".srg-pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 && killLeftover(pid, dir) {
				n.logger.Warn("gc: killed leftover runner process", "pid", pid, "dir", dir)
			}
		}
		err = removeAll(dir)
		n.release(dir)
		if err != nil {
			res.Leftovers = append(res.Leftovers, dir)
			n.logger.Warn("gc: leftover job dir not removed", "dir", dir, "error", err)
			continue
		}
		res.Removed = append(res.Removed, dir)
	}
	return res, nil
}

// Root is native.root.
func (n *Native) Root() string { return n.cfg.Root }

// claim marks dir as busy for one actor (start, stop or the sweep). It fails when a live runner owns dir or another
// actor holds it.
func (n *Native) claim(dir string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, live := n.procs[dir]; live || n.claimed[dir] {
		return false
	}
	n.claimed[dir] = true
	return true
}

func (n *Native) release(dir string) {
	n.mu.Lock()
	delete(n.claimed, dir)
	n.mu.Unlock()
}
