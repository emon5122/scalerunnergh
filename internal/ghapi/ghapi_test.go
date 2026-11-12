package ghapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestClientGets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/o/r":
			fmt.Fprint(w, `{"full_name":"o/r","private":true,"visibility":"private"}`)
		case "/repos/o/r/actions/runs/5":
			fmt.Fprint(w, `{"event":"pull_request","repository":{"full_name":"o/r"},"head_repository":{"full_name":"x/r"}}`)
		case "/repos/o/limited":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		case "/repos/o/secondary":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusForbidden)
		case "/repos/o/secondary-body":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes."}`)
		case "/repos/o/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/repos/o/busy":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/repos/o/broken":
			w.WriteHeader(http.StatusBadGateway)
		case "/orgs/o/actions/runner-groups":
			fmt.Fprint(w, `{"runner_groups":[{"id":3,"name":"vps-private","allows_public_repositories":false}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tokens: StaticToken("tok")}
	ctx := context.Background()
	if r, err := c.GetRepo(ctx, "o", "r"); err != nil || !r.Private || r.FullName != "o/r" {
		t.Errorf("repo: %+v %v", r, err)
	}
	if r, err := c.GetRun(ctx, "o", "r", 5); err != nil || r.HeadRepository.FullName != "x/r" {
		t.Errorf("run: %+v %v", r, err)
	}
	if g, err := c.ListRunnerGroups(ctx, "o"); err != nil || len(g) != 1 || g[0].Name != "vps-private" {
		t.Errorf("groups: %+v %v", g, err)
	}
	if _, err := c.GetRepo(ctx, "o", "missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("want 404 error, got %v", err)
	}
	for repo, want := range map[string][2]bool{ // {IsDenied, IsNotFound}
		"forbidden": {true, false}, "limited": {false, false}, "secondary": {false, false}, "secondary-body": {false, false},
		"busy": {false, false}, "broken": {false, false}, "missing": {false, true},
	} {
		_, err := c.GetRepo(ctx, "o", repo)
		if err == nil || IsDenied(err) != want[0] || IsNotFound(err) != want[1] {
			t.Errorf("%s: denied=%v notfound=%v err=%v", repo, IsDenied(err), IsNotFound(err), err)
		}
	}
	bad := &Client{BaseURL: srv.URL, Tokens: StaticToken("secret-value")}
	// 401: the credential is broken; retryable, not a denial, and never echoed.
	if _, err := bad.GetRepo(ctx, "o", "r"); err == nil || IsDenied(err) || IsNotFound(err) || strings.Contains(err.Error(), "secret-value") {
		t.Errorf("error must not contain the token: %v", err)
	}
}

func TestAppToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := jwt.RegisteredClaims{}
		if _, err := jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }); err != nil || claims.Issuer != "Iv1.abc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_inst","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()
	a := &AppToken{ClientID: "Iv1.abc", InstallationID: 42, PrivateKeyPEM: pemKey, BaseURL: srv.URL}
	for i := 0; i < 3; i++ {
		tok, err := a.Token(context.Background())
		if err != nil || tok != "ghs_inst" {
			t.Fatalf("token: %q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Errorf("token must be cached, got %d exchanges", calls)
	}
	if _, err := (&AppToken{ClientID: "x", InstallationID: 1, PrivateKeyPEM: "not a key"}).Token(context.Background()); err == nil {
		t.Error("bad key must fail")
	}
}
