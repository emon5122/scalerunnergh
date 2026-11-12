// Package backend starts one ephemeral GitHub Actions runner per job.
//
// Two implementations exist: docker (a container per job, Linux hosts) and
// native (a runner process per job in a clean directory, Linux and Windows).
// The scaler only sees this interface.
package backend

import (
	"context"

	"github.com/emon5122/scalerunnergh/internal/config"
)

// RunnerSpec describes one runner to start.
type RunnerSpec struct {
	// Name is the runner name registered at GitHub; it is unique per runner.
	Name string
	// ScaleSet is the scale set name (for labels and logs).
	ScaleSet string
	// JITConfig is the single-use encoded JIT runner config. It is a secret:
	// never log it and never put it on a command line.
	JITConfig string
	// Image and DinD apply to the docker backend.
	Image string
	DinD  bool
	// Limits per runner.
	Limits config.Limits
}

// Handle identifies a started runner inside its backend.
type Handle struct {
	// ID is the container id or the job directory.
	ID string
	// Name is RunnerSpec.Name.
	Name string
}

// Backend launches and removes ephemeral runners.
type Backend interface {
	// Name is "docker" or "native".
	Name() string
	// Start launches one ephemeral runner and returns its handle.
	Start(ctx context.Context, spec RunnerSpec) (Handle, error)
	// Wait blocks until the runner exits (it does after one job).
	Wait(ctx context.Context, h Handle) (exitCode int, err error)
	// Stop force-stops the runner and removes everything it left behind.
	// It is safe to call after the runner exited and more than once.
	Stop(ctx context.Context, h Handle) error
	// Cleanup removes leftovers of a previous orchestrator process.
	Cleanup(ctx context.Context) error
}
