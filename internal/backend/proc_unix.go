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

// procTree is the runner's process group.
type procTree struct{ pgid int }

func afterStart(cmd *exec.Cmd, nice int) procTree {
	if nice > 0 {
		_ = syscall.Setpriority(syscall.PRIO_PGRP, cmd.Process.Pid, nice)
	}
	return procTree{pgid: cmd.Process.Pid}
}

// kill signals every process of the group. A group id stays reserved while
// any member is alive, so this cannot hit an unrelated process.
func (t procTree) kill(force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-t.pgid, sig)
}

func (t procTree) close() {}

// killLeftover kills the process group of a runner left by an earlier
// orchestrator process. It acts only if pid still runs from dir (pid reuse).
func killLeftover(pid int, dir string) bool {
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return false
	}
	if cwd != dir && !strings.HasPrefix(cwd, dir+string(filepath.Separator)) {
		return false
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return true
}
