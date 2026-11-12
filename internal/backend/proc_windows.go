//go:build windows

package backend

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	createNewProcessGroup    = 0x00000200
	belowNormalPriorityClass = 0x00004000
)

func runnerCommand(dir string) *exec.Cmd {
	return exec.Command("cmd.exe", "/d", "/c", filepath.Join(dir, "run.cmd"))
}

// setProcAttrs starts the runner in a new process group, at below-normal
// priority when nice > 0.
func setProcAttrs(cmd *exec.Cmd, nice int) {
	flags := uint32(createNewProcessGroup)
	if nice > 0 {
		flags |= belowNormalPriorityClass
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}

func afterStart(cmd *exec.Cmd, nice int) {}

// killTree ends the runner and every process it started.
func killTree(pid int, force bool) {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// isRunnerProc reports whether pid still is a cmd.exe/runner process
// (guards against pid reuse after a crash).
func isRunnerProc(pid int, dir string) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	s := strings.ToLower(string(out))
	return strings.Contains(s, "cmd.exe") || strings.Contains(s, "runner.listener")
}
