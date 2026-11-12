//go:build !windows

package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func runnerCommand(dir string) *exec.Cmd {
	return exec.Command(filepath.Join(dir, "run.sh"))
}

// setProcAttrs puts the runner in its own process group so the whole job
// tree can be signalled.
func setProcAttrs(cmd *exec.Cmd, nice int) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func afterStart(cmd *exec.Cmd, nice int) {
	if nice > 0 {
		_ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, nice)
	}
}

// killTree signals the runner's process group.
func killTree(pid int, force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-pid, sig)
}

// isRunnerProc reports whether pid still is a runner started from dir
// (guards against pid reuse after a crash).
func isRunnerProc(pid int, dir string) bool {
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return false
	}
	return cwd == dir || strings.HasPrefix(cwd, dir+string(filepath.Separator))
}
