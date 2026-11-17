package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/emon5122/scalerunnergh/internal/config"
)

// authDocker is a Docker whose image registry has a credential, over the fake daemon. The fake strips the
// --config/-H global flags an authed pull puts first and records what the temporary config held at pull time.
type authSeen struct {
	configDir string
	auth      string
	host      string
	argv      []string
}

func authDocker(t *testing.T, f *fakeDaemon, endpoint string) (*Docker, *authSeen) {
	t.Helper()
	pw := filepath.Join(t.TempDir(), "ghcr.pat")
	if err := os.WriteFile(pw, []byte("ghp_SECRETTOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewDocker(config.DockerBackendConfig{Binary: "docker", RegistryAuth: []config.RegistryAuth{
		{Registry: "ghcr.io", Username: "emon5122", PasswordFile: pw}}}, "h")
	seen := &authSeen{}
	d.run = func(ctx context.Context, stdin []byte, args ...string) (string, error) {
		seen.argv = append(seen.argv, strings.Join(args, " "))
		if len(args) > 2 && args[0] == "context" {
			return endpoint, nil
		}
		for len(args) > 0 && (args[0] == "--config" || args[0] == "-H") {
			if args[0] == "--config" {
				seen.configDir = args[1]
				b, err := os.ReadFile(filepath.Join(args[1], "config.json"))
				if err != nil {
					t.Errorf("config.json missing while pulling: %v", err)
				}
				var c struct {
					Auths map[string]struct{ Auth string } `json:"auths"`
				}
				_ = json.Unmarshal(b, &c)
				seen.auth = c.Auths["ghcr.io"].Auth
				if fi, _ := os.Stat(filepath.Join(args[1], "config.json")); fi != nil && fi.Mode().Perm() != 0o600 {
					t.Errorf("config.json mode %v", fi.Mode())
				}
			} else {
				seen.host = args[1]
			}
			args = args[2:]
		}
		return f.run(ctx, stdin, args...)
	}
	return d, seen
}

func TestRegistryAuthPullUsesATemporaryConfig(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "img1"
	d, seen := authDocker(t, f, "unix:///run/user/1000/docker.sock")
	if err := d.Pull(context.Background(), testImage); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString([]byte("emon5122:ghp_SECRETTOKEN"))
	if seen.auth != want {
		t.Fatalf("auth in the temporary config %q", seen.auth)
	}
	if seen.host != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("daemon endpoint not pinned: %q", seen.host)
	}
	if _, err := os.Stat(seen.configDir); !os.IsNotExist(err) {
		t.Fatalf("temporary config dir left behind: %v", err)
	}
	for _, a := range seen.argv {
		if strings.Contains(a, "SECRETTOKEN") || strings.Contains(a, want) {
			t.Fatalf("the credential reached an argv: %s", a)
		}
	}
}

func TestRegistryAuthCreateNeverPullsAndMissingImageIsPulledFirst(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "img1"
	d, seen := authDocker(t, f, "")
	sp := spec()
	sp.Image = testImage // pull policy "missing" (PullAlways false)
	if _, err := d.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	if f.count("pull") != 1 || seen.auth == "" {
		t.Fatalf("missing image not pulled with the credential before create: %d pulls", f.count("pull"))
	}
	create := ""
	for _, c := range seen.argv {
		if strings.HasPrefix(c, "create") {
			create = c
		}
	}
	if !strings.Contains(create, "--pull never "+testImage) {
		t.Fatalf("create without --pull never: %s", create)
	}
	if seen.host != "" {
		t.Fatalf("no endpoint expected: %q", seen.host)
	}
	// the image is now local: the next start pulls nothing
	sp.Name = "r2"
	if _, err := d.Start(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	if f.count("pull") != 1 {
		t.Fatalf("a present image was pulled again: %d", f.count("pull"))
	}
}

func TestRegistryAuthOnlyForItsRegistry(t *testing.T) {
	f := newFakeDaemon()
	other := "docker.io/library/alpine:3"
	f.remote[other] = "img2"
	d, seen := authDocker(t, f, "")
	if err := d.Pull(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if seen.configDir != "" {
		t.Fatal("a pull from another registry used the credential")
	}
	if args := d.CreateArgs(RunnerSpec{Name: "x", Image: other}); slices.Contains(args, "never") {
		t.Fatalf("--pull never for an image without registry_auth: %v", args)
	}
}

func TestRegistryAuthRefusesATCPEndpoint(t *testing.T) {
	f := newFakeDaemon()
	f.remote[testImage] = "img1"
	d, _ := authDocker(t, f, "tcp://10.0.0.5:2376")
	if err := d.Pull(context.Background(), testImage); err == nil || !strings.Contains(err.Error(), "local docker endpoint") {
		t.Fatalf("tcp endpoint: %v", err)
	}
}

func TestSubcommandLabel(t *testing.T) {
	if s := subcommand([]string{"--config", "/tmp/x", "-H", "unix:///s", "pull", "-q", "i"}); s != "pull" {
		t.Fatal(s)
	}
	if s := subcommand([]string{"create", "--name", "n"}); s != "create" {
		t.Fatal(s)
	}
}

func TestRegistryOf(t *testing.T) {
	for in, want := range map[string]string{"ghcr.io/o/i:t": "ghcr.io", "alpine:3": "docker.io", "library/alpine": "docker.io",
		"localhost/x": "localhost", "reg.example.com:5000/a/b": "reg.example.com:5000"} {
		if got := config.RegistryOf(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
