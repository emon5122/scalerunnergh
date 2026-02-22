// Package runner provides the real Docker client implementation using
// command-line docker invocations.
package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CLIDockerClient implements DockerClient using the docker CLI.
type CLIDockerClient struct{}

// NewCLIDockerClient returns a DockerClient backed by the docker CLI.
func NewCLIDockerClient() *CLIDockerClient {
	return &CLIDockerClient{}
}

func (c *CLIDockerClient) CreateAndStart(ctx context.Context, opts ContainerOpts) (string, error) {
	args := []string{"run", "-d"}

	if opts.AutoRemove {
		args = append(args, "--rm")
	}

	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}

	for k, v := range opts.Env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	for k, v := range opts.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	if opts.GPUEnabled {
		args = append(args, "--gpus", "all")
	}

	if opts.NetworkMode != "" {
		args = append(args, "--network", opts.NetworkMode)
	}

	args = append(args, opts.ExtraArgs...)
	args = append(args, opts.Image)

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run: %w: %s", err, string(out))
	}

	containerID := strings.TrimSpace(string(out))
	return containerID, nil
}

func (c *CLIDockerClient) Wait(ctx context.Context, containerID string) (int, error) {
	cmd := exec.CommandContext(ctx, "docker", "wait", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return -1, fmt.Errorf("docker wait: %w: %s", err, string(out))
	}

	var exitCode int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &exitCode); err != nil {
		return -1, fmt.Errorf("parse exit code: %w", err)
	}
	return exitCode, nil
}

func (c *CLIDockerClient) Remove(ctx context.Context, containerID string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, string(out))
	}
	return nil
}

func (c *CLIDockerClient) IsRunning(ctx context.Context, containerID string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}", containerID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("docker inspect: %w: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
