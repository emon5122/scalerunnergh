package backend

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/emon5122/scalerunnergh/internal/config"
)

// JITPathInContainer is where the runner image's entrypoint reads the JIT
// config from (and deletes it before the job starts).
const JITPathInContainer = "/srg/jitconfig"

// Docker runs each runner in its own container through the docker CLI.
type Docker struct {
	cfg  config.DockerBackendConfig
	host string
	// run executes the docker CLI; tests replace it.
	run func(ctx context.Context, stdin []byte, args ...string) (string, error)
}

// NewDocker creates the docker backend for a host.
func NewDocker(cfg config.DockerBackendConfig, host string) *Docker {
	d := &Docker{cfg: cfg, host: host}
	d.run = d.exec
	return d
}

// Name implements Backend.
func (d *Docker) Name() string { return config.BackendDocker }

func (d *Docker) exec(ctx context.Context, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.cfg.Binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CreateArgs builds the docker create arguments. The JIT config is not in them.
func (d *Docker) CreateArgs(spec RunnerSpec) []string {
	args := []string{"create", "--name", spec.Name,
		"--label", "scalerunner=true",
		"--label", "scalerunner.host=" + d.host,
		"--label", "scalerunner.scaleset=" + spec.ScaleSet,
		"--init",
	}
	l := spec.Limits
	if l.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(l.CPUs, 'f', -1, 64))
	}
	if l.MemoryMB > 0 {
		m := strconv.Itoa(l.MemoryMB) + "m"
		args = append(args, "--memory", m, "--memory-swap", m) // no swap on top
	}
	if l.Pids > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(l.Pids))
	}
	if l.CPUShares > 0 {
		args = append(args, "--cpu-shares", strconv.Itoa(l.CPUShares))
	}
	if l.BlkioWeight > 0 {
		args = append(args, "--blkio-weight", strconv.Itoa(l.BlkioWeight))
	}
	if l.OOMScoreAdj > 0 {
		args = append(args, "--oom-score-adj", strconv.Itoa(l.OOMScoreAdj))
	}
	if l.StorageSize != "" {
		args = append(args, "--storage-opt", "size="+l.StorageSize)
	}
	if l.GPUs != "" {
		args = append(args, "--gpus", l.GPUs)
	}
	if spec.DinD {
		// A private dockerd inside this container; its data is an anonymous
		// volume removed with the container.
		args = append(args, "--privileged", "-e", "SRG_DIND=1", "-v", "/var/lib/docker")
		if d.cfg.RegistryMirror != "" {
			args = append(args, "-e", "SRG_REGISTRY_MIRROR="+d.cfg.RegistryMirror)
		}
	}
	if d.cfg.Network != "" {
		args = append(args, "--network", d.cfg.Network)
	}
	if d.cfg.Runtime != "" {
		args = append(args, "--runtime", d.cfg.Runtime)
	}
	args = append(args, d.cfg.ExtraArgs...)
	return append(args, spec.Image)
}

// Start implements Backend: create, copy the JIT config in as a file, start.
func (d *Docker) Start(ctx context.Context, spec RunnerSpec) (Handle, error) {
	id, err := d.run(ctx, nil, d.CreateArgs(spec)...)
	if err != nil {
		return Handle{}, err
	}
	h := Handle{ID: id, Name: spec.Name}
	archive, err := jitTar(spec.JITConfig)
	if err == nil {
		_, err = d.run(ctx, archive, "cp", "-", id+":/")
	}
	if err == nil {
		_, err = d.run(ctx, nil, "start", id)
	}
	if err != nil {
		_ = d.Stop(context.WithoutCancel(ctx), h)
		return Handle{}, err
	}
	return h, nil
}

// jitTar packs the JIT config as /srg/jitconfig, readable by root only; the
// image entrypoint (root) reads and deletes it.
func jitTar(jit string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "srg/", Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(JITPathInContainer, "/")
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(jit))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write([]byte(jit)); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Wait implements Backend.
func (d *Docker) Wait(ctx context.Context, h Handle) (int, error) {
	out, err := d.run(ctx, nil, "wait", h.ID)
	if err != nil {
		return -1, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return -1, fmt.Errorf("docker wait: unexpected output %q", out)
	}
	return code, nil
}

// Stop implements Backend: remove the container and its anonymous volumes.
func (d *Docker) Stop(ctx context.Context, h Handle) error {
	_, err := d.run(ctx, nil, "rm", "-f", "-v", h.ID)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

// Cleanup removes every runner container of this host left by an earlier process.
func (d *Docker) Cleanup(ctx context.Context) error {
	out, err := d.run(ctx, nil, "ps", "-aq", "--filter", "label=scalerunner.host="+d.host)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil
	}
	_, err = d.run(ctx, nil, append([]string{"rm", "-f", "-v"}, ids...)...)
	return err
}
