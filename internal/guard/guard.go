// Package guard keeps public repositories and fork pull requests away from
// the runners. It is the orchestrator's own check on top of the org setting
// (no fork-PR workflows) and the private runner group. Every check fails
// closed: if GitHub cannot be asked, the job is not taken.
package guard

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emon5122/scalerunnergh/internal/ghapi"
)

// API is the part of ghapi.Client the guard uses.
type API interface {
	GetRepo(ctx context.Context, owner, repo string) (*ghapi.Repo, error)
	GetRun(ctx context.Context, owner, repo string, runID int64) (*ghapi.Run, error)
	ListRunnerGroups(ctx context.Context, org string) ([]ghapi.RunnerGroup, error)
}

// Guard checks scale sets and jobs.
type Guard struct {
	api        API
	allowRepos map[string]bool // lower-case "owner/repo"; empty = any private repo
	ttl        time.Duration
	now        func() time.Time
	// notFoundRetry is the pause before a missing workflow run is asked for again.
	notFoundRetry time.Duration

	mu    sync.Mutex
	repos map[string]repoEntry
}

type repoEntry struct {
	private bool
	at      time.Time
}

// New creates a guard. allowRepos entries are "repo" (in owner) or "owner/repo".
func New(api API, owner string, allowRepos []string) *Guard {
	allow := map[string]bool{}
	for _, r := range allowRepos {
		if !strings.Contains(r, "/") {
			r = owner + "/" + r
		}
		allow[strings.ToLower(r)] = true
	}
	return &Guard{api: api, allowRepos: allow, ttl: 10 * time.Minute, now: time.Now, notFoundRetry: 2 * time.Second, repos: map[string]repoEntry{}}
}

// CheckTarget verifies where the scale set registers: an org runner group
// must refuse public repositories; a repo-level scale set needs a private repo.
func (g *Guard) CheckTarget(ctx context.Context, owner, repo, group string) error {
	if repo != "" {
		private, err := g.repoPrivate(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("guard: %w", err)
		}
		if !private {
			return fmt.Errorf("guard: %s/%s is public; runners are for private repos only", owner, repo)
		}
		return nil
	}
	groups, err := g.api.ListRunnerGroups(ctx, owner)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	for _, rg := range groups {
		if !strings.EqualFold(rg.Name, group) {
			continue
		}
		if rg.AllowsPublicRepositories {
			return fmt.Errorf("guard: runner group %q of %s allows public repositories; turn that off", group, owner)
		}
		return nil
	}
	return fmt.Errorf("guard: runner group %q not found in %s", group, owner)
}

// AllowJob decides whether a job may run here. allowed=false with err=nil is
// a definite refusal (public repo, repo off the allow list, a run whose head
// is another repository, i.e. a fork). err != nil means GitHub could not be
// asked; the job must not run, and the caller should retry later.
//
// The decision never depends on the event name in the message: the workflow
// run is always fetched and its head repository compared with the repository.
func (g *Guard) AllowJob(ctx context.Context, owner, repo string, runID int64) (allowed bool, reason string, err error) {
	if owner == "" || repo == "" {
		return false, "job has no repository", nil
	}
	if runID == 0 {
		return false, "job has no workflow run id", nil
	}
	full := strings.ToLower(owner + "/" + repo)
	if len(g.allowRepos) > 0 && !g.allowRepos[full] {
		return false, "repository not in allow_repos", nil
	}
	private, err := g.repoPrivate(ctx, owner, repo)
	if ghapi.IsNotFound(err) {
		// Gone, or not visible to this credential: a definite no, not a retry.
		return false, "repository not found", nil
	}
	if err != nil {
		return false, "cannot verify repository", fmt.Errorf("guard: %w", err)
	}
	if !private {
		return false, "public repository", nil
	}
	run, err := g.api.GetRun(ctx, owner, repo, runID)
	if ghapi.IsNotFound(err) {
		// A run created a moment ago can be missing briefly: one retry.
		select {
		case <-ctx.Done():
			return false, "cannot verify workflow run", fmt.Errorf("guard: %w", ctx.Err())
		case <-time.After(g.notFoundRetry):
		}
		run, err = g.api.GetRun(ctx, owner, repo, runID)
	}
	if ghapi.IsNotFound(err) {
		return false, "workflow run not found", nil
	}
	if err != nil {
		return false, "cannot verify workflow run", fmt.Errorf("guard: %w", err)
	}
	if run.HeadRepository == nil || run.Repository == nil ||
		!strings.EqualFold(run.HeadRepository.FullName, run.Repository.FullName) ||
		!strings.EqualFold(run.Repository.FullName, owner+"/"+repo) {
		return false, "run head is another repository (fork)", nil
	}
	return true, "", nil
}

func (g *Guard) repoPrivate(ctx context.Context, owner, repo string) (bool, error) {
	key := strings.ToLower(owner + "/" + repo)
	g.mu.Lock()
	e, ok := g.repos[key]
	g.mu.Unlock()
	if ok && g.now().Sub(e.at) < g.ttl {
		return e.private, nil
	}
	r, err := g.api.GetRepo(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	private := r.Private && r.Visibility != "public"
	g.mu.Lock()
	g.repos[key] = repoEntry{private: private, at: g.now()}
	g.mu.Unlock()
	return private, nil
}
