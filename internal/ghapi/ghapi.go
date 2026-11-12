// Package ghapi is a small GitHub REST client for the few read-only calls the
// safety guard needs (repo visibility, runner groups, workflow runs). It
// authenticates with a PAT or a GitHub App installation token.
package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenSource yields a bearer token for the REST API.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a PAT.
type StaticToken string

// Token returns the PAT.
func (s StaticToken) Token(context.Context) (string, error) { return string(s), nil }

// AppToken exchanges a GitHub App JWT for an installation token and caches it.
type AppToken struct {
	ClientID       string
	InstallationID int64
	PrivateKeyPEM  string
	BaseURL        string // default https://api.github.com
	HTTP           *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached installation token, refreshing it 5 minutes early.
func (a *AppToken) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 5*time.Minute {
		return a.token, nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(a.PrivateKeyPEM))
	if err != nil {
		return "", fmt.Errorf("github app key: %w", err)
	}
	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(8 * time.Minute)),
		Issuer:    a.ClientID,
	}).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("github app jwt: %w", err)
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/app/installations/%d/access_tokens", base, a.InstallationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("github app token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("github app token: status %d", resp.StatusCode)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("github app token: %w", err)
	}
	a.token, a.expires = out.Token, out.ExpiresAt
	return a.token, nil
}

// StatusError is a non-200 answer from the REST API.
type StatusError struct {
	Path   string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("github GET %s: status %d", e.Path, e.Status)
}

// IsNotFound reports whether err says the resource does not exist (404/410):
// a definite answer, unlike a network error or a rate limit.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusGone)
}

// Client calls the REST API.
type Client struct {
	BaseURL string // default https://api.github.com
	Tokens  TokenSource
	HTTP    *http.Client
}

// Repo is the part of a repository the guard needs.
type Repo struct {
	FullName   string `json:"full_name"`
	Private    bool   `json:"private"`
	Visibility string `json:"visibility"`
}

// RunnerGroup is an org runner group.
type RunnerGroup struct {
	ID                       int    `json:"id"`
	Name                     string `json:"name"`
	Visibility               string `json:"visibility"`
	AllowsPublicRepositories bool   `json:"allows_public_repositories"`
}

// Run is the part of a workflow run the guard needs.
type Run struct {
	Event          string `json:"event"`
	Repository     *Repo  `json:"repository"`
	HeadRepository *Repo  `json:"head_repository"`
}

// GetRepo fetches a repository.
func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*Repo, error) {
	var r Repo
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repo), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRun fetches a workflow run.
func (c *Client) GetRun(ctx context.Context, owner, repo string, runID int64) (*Run, error) {
	var r Run
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d", owner, repo, runID), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRunnerGroups lists an org's runner groups (first 100).
func (c *Client) ListRunnerGroups(ctx context.Context, org string) ([]RunnerGroup, error) {
	var out struct {
		Groups []RunnerGroup `json:"runner_groups"`
	}
	if err := c.get(ctx, fmt.Sprintf("/orgs/%s/actions/runner-groups?per_page=100", org), &out); err != nil {
		return nil, err
	}
	return out.Groups, nil
}

func (c *Client) get(ctx context.Context, path string, dest any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	tok, err := c.Tokens.Token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("github GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return &StatusError{Path: path, Status: resp.StatusCode}
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}
