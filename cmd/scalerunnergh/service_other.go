//go:build !windows

package main

import "context"

// runMaybeService runs fn directly; service managers (systemd, Docker) need nothing special.
func runMaybeService(ctx context.Context, cancel context.CancelFunc, fn func(context.Context) error) error {
	return fn(ctx)
}
