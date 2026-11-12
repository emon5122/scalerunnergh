//go:build windows

package main

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "scalerunnergh"

// runMaybeService runs fn under the Windows Service Control Manager when the
// process was started as a service, and directly otherwise (console).
func runMaybeService(ctx context.Context, cancel context.CancelFunc, fn func(context.Context) error) error {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return fn(ctx)
	}
	h := &handler{ctx: ctx, cancel: cancel, fn: fn}
	if err := svc.Run(serviceName, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	ctx    context.Context
	cancel context.CancelFunc
	fn     func(context.Context) error
	err    error
}

// Execute implements svc.Handler: a stop or shutdown request cancels the
// context, and the app stops its runners before the service reports stopped.
func (h *handler) Execute(_ []string, reqs <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	done := make(chan error, 1)
	go func() { done <- h.fn(h.ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			h.err = err
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1
			}
			return false, 0
		case r := <-reqs:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 180000}
				h.cancel()
			}
		}
	}
}
