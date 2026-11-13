package backend

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emon5122/scalerunnergh/internal/config"
)

type call struct {
	args  []string
	stdin []byte
}

func fakeDocker(d *Docker, fail string) *[]call {
	var calls []call
	d.run = func(_ context.Context, stdin []byte, args ...string) (string, error) {
		calls = append(calls, call{args, stdin})
		if args[0] == fail {
			return "", errors.New("docker " + fail + ": boom")
		}
		switch args[0] {
		case "create":
			return "cid123", nil
		case "wait":
			return "7", nil
		case "ps":
			return "old1\nold2", nil
		}
		return "", nil
	}
	return &calls
}

const secretJIT = "SECRET-JIT-CONFIG"

func spec() RunnerSpec {
	return RunnerSpec{Name: "srg-h-s-abcd", ScaleSet: "s", JITConfig: secretJIT, Image: "img:1", DinD: true,
		Limits: config.Limits{CPUs: 2.5, MemoryMB: 3072, Pids: 2048, CPUShares: 128, BlkioWeight: 100, OOMScoreAdj: 1000, StorageSize: "40G", GPUs: "all"}}
}

func TestDockerCreateArgs(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker", Network: "srg", Runtime: "sysbox-runc", RegistryMirror: "http://mirror:5000", ExtraArgs: []string{"--dns", "1.1.1.1"}}, "h")
	got := strings.Join(d.CreateArgs(spec()), " ")
	for _, want := range []string{
		"create --name srg-h-s-abcd", "--label scalerunner.host=h", "--label scalerunner.scaleset=s",
		"--cpus 2.5", "--memory 3072m --memory-swap 3072m", "--pids-limit 2048", "--cpu-shares 128",
		"--blkio-weight 100", "--oom-score-adj 1000", "--storage-opt size=40G", "--gpus all",
		"--privileged -e SRG_DIND=1 -v /var/lib/docker", "-e SRG_REGISTRY_MIRROR=http://mirror:5000",
		"--network srg", "--runtime sysbox-runc", "--dns 1.1.1.1 img:1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	if strings.Contains(got, secretJIT) || strings.Contains(got, "docker.sock") {
		t.Errorf("JIT config or host socket in args: %s", got)
	}

	plain := spec()
	plain.DinD, plain.Limits = false, config.Limits{}
	got = strings.Join(NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h").CreateArgs(plain), " ")
	for _, bad := range []string{"--privileged", "--memory", "--cpus", "SRG_DIND", "--network", "--gpus"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in: %s", bad, got)
		}
	}
}

func TestDockerStartCopiesJITAsFile(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	calls := fakeDocker(d, "")
	h, err := d.Start(context.Background(), spec())
	if err != nil || h.ID != "srg-h-s-abcd" {
		t.Fatalf("start: %+v %v", h, err)
	}
	if len(*calls) != 3 || (*calls)[1].args[0] != "cp" || (*calls)[2].args[0] != "start" {
		t.Fatalf("calls: %+v", *calls)
	}
	for _, c := range *calls {
		if strings.Contains(strings.Join(c.args, " "), secretJIT) {
			t.Fatalf("JIT config on a command line: %v", c.args)
		}
	}
	tr := tar.NewReader(bytes.NewReader((*calls)[1].stdin))
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if "/"+hdr.Name == JITPathInContainer {
			b, _ := io.ReadAll(tr)
			found = string(b) == secretJIT && hdr.Mode == 0o600
		}
	}
	if !found {
		t.Fatal("JIT config file (mode 0600) not in the copied archive")
	}
	if code, err := d.Wait(context.Background(), h); err != nil || code != 7 {
		t.Fatalf("wait: %d %v", code, err)
	}
}

func TestDockerStartFailureRemovesContainer(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	calls := fakeDocker(d, "start")
	if _, err := d.Start(context.Background(), spec()); err == nil {
		t.Fatal("start must fail")
	}
	last := (*calls)[len(*calls)-1].args
	if strings.Join(last, " ") != "rm -f -v srg-h-s-abcd" {
		t.Fatalf("container not removed after failed start: %v", last)
	}
}

func TestDockerWaitSurvivesTransientErrors(t *testing.T) {
	old := waitRetry
	waitRetry = time.Millisecond
	defer func() { waitRetry = old }()
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	waits := 0
	d.run = func(_ context.Context, _ []byte, args ...string) (string, error) {
		switch args[0] {
		case "wait":
			waits++
			if waits < 3 {
				return "", errors.New("docker wait: daemon restarting")
			}
			return "0", nil
		case "inspect":
			return "true", nil
		}
		return "", nil
	}
	if code, err := d.Wait(context.Background(), Handle{ID: "c"}); err != nil || code != 0 || waits != 3 {
		t.Fatalf("wait must retry while the container runs: code=%d err=%v waits=%d", code, err, waits)
	}
	d.run = func(_ context.Context, _ []byte, args ...string) (string, error) {
		if args[0] == "inspect" {
			return "false", nil
		}
		return "", errors.New("docker wait: boom")
	}
	if _, err := d.Wait(context.Background(), Handle{ID: "c"}); err == nil {
		t.Fatal("wait must fail once the container stopped")
	}
	// Daemon down: wait and inspect both fail; keep waiting, then it comes back.
	calls := 0
	d.run = func(_ context.Context, _ []byte, args ...string) (string, error) {
		calls++
		if calls <= 6 {
			return "", errors.New("Cannot connect to the Docker daemon")
		}
		if args[0] == "wait" {
			return "0", nil
		}
		return "true", nil
	}
	if code, err := d.Wait(context.Background(), Handle{ID: "c"}); err != nil || code != 0 {
		t.Fatalf("wait must outlast a daemon restart: code=%d err=%v", code, err)
	}
	d.run = func(_ context.Context, _ []byte, args ...string) (string, error) {
		if args[0] == "inspect" {
			return "", errors.New("Error: No such object: c")
		}
		return "", errors.New("docker wait: No such container: c")
	}
	if _, err := d.Wait(context.Background(), Handle{ID: "c"}); err == nil {
		t.Fatal("wait must fail once the container is gone")
	}
}

func TestDockerCleanup(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "build-host")
	calls := fakeDocker(d, "")
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join((*calls)[0].args, " "); got != "ps -aq --filter label=scalerunner.host=build-host" {
		t.Errorf("ps: %s", got)
	}
	if got := strings.Join((*calls)[1].args, " "); got != "rm -f -v old1 old2" {
		t.Errorf("rm: %s", got)
	}
}

func TestDockerStopIgnoresMissing(t *testing.T) {
	d := NewDocker(config.DockerBackendConfig{Binary: "docker"}, "h")
	d.run = func(context.Context, []byte, ...string) (string, error) {
		return "", errors.New("docker rm: exit 1: Error response from daemon: No such container: x")
	}
	if err := d.Stop(context.Background(), Handle{ID: "x"}); err != nil {
		t.Fatalf("stop of a removed container must succeed: %v", err)
	}
}

func TestFilterEnv(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "GITHUB_PAT=x", "SRG_FOO=1", "MY_TOKEN=x", "db_password=x", "AWS_SECRET_ACCESS_KEY=x",
		"SSH_KEY=x", "PAT=x", "ACTIONS_RUNNER_INPUT_JITCONFIG=old", "KEYBOARD=us", "LANG=C"}
	got := strings.Join(FilterEnv(in), ",")
	if got != "PATH=/bin,HOME=/h,KEYBOARD=us,LANG=C" {
		t.Fatalf("filtered env: %s", got)
	}
}
