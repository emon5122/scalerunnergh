package guard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/emon5122/scalerunnergh/internal/ghapi"
)

type fakeAPI struct {
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
		return nil, errors.New("status 404")
	}
	return r, nil
}

func (f *fakeAPI) GetRun(_ context.Context, _, _ string, id int64) (*ghapi.Run, error) {
	if f.err != nil {
		return nil, f.err
	}
	r, ok := f.runs[id]
	if !ok {
		return nil, errors.New("status 404")
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
		name, repo, event string
		run               int64
		ok                bool
		reason            string
	}{
		{"private push", "app", "push", 0, true, ""},
		{"internal repo", "inner", "push", 0, true, ""},
		{"public repo", "public", "push", 0, false, "public repository"},
		{"same-repo PR", "app", "pull_request", 1, true, ""},
		{"fork PR", "app", "pull_request", 2, false, "fork"},
		{"fork PR target", "app", "pull_request_target", 2, false, "fork"},
		{"PR without head repo", "app", "pull_request", 3, false, "fork"},
		{"PR run unknown", "app", "pull_request", 99, false, "cannot verify pull request"},
		{"unknown repo", "ghost", "push", 0, false, "cannot verify repository"},
		{"no repo", "", "push", 0, false, "no repository"},
	}
	for _, c := range cases {
		g := New(newFake(), "org", nil)
		ok, reason := g.AllowJob(context.Background(), "org", c.repo, c.event, c.run)
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: got ok=%v reason=%q", c.name, ok, reason)
		}
	}
}

func TestAllowReposAndCache(t *testing.T) {
	api := newFake()
	g := New(api, "org", []string{"APP", "other/thing"})
	if ok, _ := g.AllowJob(context.Background(), "org", "app", "push", 0); !ok {
		t.Error("allow-listed repo must pass (case-insensitive)")
	}
	if ok, reason := g.AllowJob(context.Background(), "org", "inner", "push", 0); ok || reason != "repository not in allow_repos" {
		t.Errorf("repo off the list must be refused: %v %q", ok, reason)
	}
	g.AllowJob(context.Background(), "org", "app", "push", 0)
	if api.repoGets != 1 {
		t.Errorf("repo visibility should be cached, got %d calls", api.repoGets)
	}
}

func TestFailsClosedOnAPIError(t *testing.T) {
	api := newFake()
	api.err = errors.New("boom")
	g := New(api, "org", nil)
	if ok, _ := g.AllowJob(context.Background(), "org", "app", "push", 0); ok {
		t.Error("API error must refuse the job")
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
