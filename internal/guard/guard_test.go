package guard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/ghapi"
)

type fakeAPI struct {
	mu       sync.Mutex
	repos    map[string]*ghapi.Repo
	runs     map[int64]*ghapi.Run
	groups   []ghapi.RunnerGroup
	err      error
	repoGets int
}

func (f *fakeAPI) GetRepo(_ context.Context, owner, repo string) (*ghapi.Repo, error) {
	f.repoGets++
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.repos[owner+"/"+repo]
	if !ok {
		return nil, &ghapi.StatusError{Path: "/repos/" + owner + "/" + repo, Status: 404}
	}
	return r, nil
}

func (f *fakeAPI) GetRun(_ context.Context, _, _ string, id int64) (*ghapi.Run, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	r, ok := f.runs[id]
	f.mu.Unlock()
	if !ok {
		return nil, &ghapi.StatusError{Path: "/runs", Status: 404}
	}
	return r, nil
}

func (f *fakeAPI) ListRunnerGroups(context.Context, string) ([]ghapi.RunnerGroup, error) {
	return f.groups, f.err
}

func newFake() *fakeAPI {
	priv := &ghapi.Repo{FullName: "org/app", Private: true, Visibility: "private"}
	return &fakeAPI{
		repos: map[string]*ghapi.Repo{
			"org/app":    priv,
			"org/public": {FullName: "org/public", Private: false, Visibility: "public"},
			"org/inner":  {FullName: "org/inner", Private: true, Visibility: "internal"},
		},
		runs: map[int64]*ghapi.Run{
			1: {Event: "pull_request", Repository: priv, HeadRepository: priv},
			2: {Event: "pull_request", Repository: priv, HeadRepository: &ghapi.Repo{FullName: "attacker/app"}},
			3: {Event: "pull_request", Repository: priv},
		},
		groups: []ghapi.RunnerGroup{
			{Name: "Default", AllowsPublicRepositories: true},
			{Name: "vps-private", AllowsPublicRepositories: false},
		},
	}
}

func TestAllowJob(t *testing.T) {
	cases := []struct {
		name, repo string
		run        int64
		ok, isErr  bool
		reason     string
	}{
		{"private repo, own run", "app", 1, true, false, ""},
		{"fork run", "app", 2, false, false, "fork"},
		{"run without head repo", "app", 3, false, false, "fork"},
		{"run of another repo", "inner", 1, false, false, "fork"},
		{"public repo", "public", 1, false, false, "public repository"},
		{"run gone (404) is a refusal, not a retry", "app", 99, false, false, "workflow run not found"},
		{"no run id", "app", 0, false, false, "no workflow run id"},
		{"repo gone (404) is a refusal, not a retry", "ghost", 1, false, false, "repository not found"},
		{"no repo", "", 1, false, false, "no repository"},
	}
	for _, c := range cases {
		g := New(newFake(), "org", nil)
		g.notFoundRetry = time.Millisecond
		ok, reason, err := g.AllowJob(context.Background(), "org", c.repo, c.run)
		if ok != c.ok || (err != nil) != c.isErr || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: got ok=%v reason=%q err=%v", c.name, ok, reason, err)
		}
	}
}

func TestRunMissingBrieflyIsRetriedOnce(t *testing.T) {
	api := newFake()
	g := New(api, "org", nil)
	g.notFoundRetry = 20 * time.Millisecond
	go func() {
		time.Sleep(5 * time.Millisecond)
		api.mu.Lock()
		api.runs[50] = api.runs[1]
		api.mu.Unlock()
	}()
	if ok, reason, err := g.AllowJob(context.Background(), "org", "app", 50); !ok || err != nil {
		t.Fatalf("a run that appears a moment later must pass: %v %q %v", ok, reason, err)
	}
}

func TestAllowReposAndCache(t *testing.T) {
	api := newFake()
	g := New(api, "org", []string{"APP", "other/thing"})
	if ok, _, _ := g.AllowJob(context.Background(), "org", "app", 1); !ok {
		t.Error("allow-listed repo must pass (case-insensitive)")
	}
	if ok, reason, _ := g.AllowJob(context.Background(), "org", "inner", 1); ok || reason != "repository not in allow_repos" {
		t.Errorf("repo off the list must be refused: %v %q", ok, reason)
	}
	g.AllowJob(context.Background(), "org", "app", 1)
	if api.repoGets != 1 {
		t.Errorf("repo visibility should be cached, got %d calls", api.repoGets)
	}
}

func TestFailsClosedOnAPIError(t *testing.T) {
	api := newFake()
	api.err = errors.New("boom")
	g := New(api, "org", nil)
	if ok, _, err := g.AllowJob(context.Background(), "org", "app", 1); ok || err == nil {
		t.Error("API error must refuse the job and report an error")
	}
	if err := g.CheckTarget(context.Background(), "org", "", "vps-private"); err == nil {
		t.Error("API error must fail the target check")
	}
}

func TestCheckTarget(t *testing.T) {
	g := New(newFake(), "org", nil)
	ctx := context.Background()
	if err := g.CheckTarget(ctx, "org", "", "VPS-Private"); err != nil {
		t.Errorf("private group: %v", err)
	}
	if err := g.CheckTarget(ctx, "org", "", "Default"); err == nil || !strings.Contains(err.Error(), "allows public") {
		t.Errorf("public-capable group must fail: %v", err)
	}
	if err := g.CheckTarget(ctx, "org", "", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing group must fail: %v", err)
	}
	if err := g.CheckTarget(ctx, "org", "app", "x"); err != nil {
		t.Errorf("private repo target: %v", err)
	}
	if err := g.CheckTarget(ctx, "org", "public", "x"); err == nil {
		t.Error("public repo target must fail")
	}
}
