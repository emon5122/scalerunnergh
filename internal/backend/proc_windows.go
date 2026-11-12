//go:build windows

package backend

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const belowNormalPriorityClass = 0x00004000

func runnerCommand(dir string) *exec.Cmd {
	return exec.Command("cmd.exe", "/d", "/c", filepath.Join(dir, "run.cmd"))
}

// setProcAttrs starts the runner in a new process group, at below-normal
// priority when nice > 0.
func setProcAttrs(cmd *exec.Cmd, nice int) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if nice > 0 {
		flags |= belowNormalPriorityClass
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
}

// procTree is a Job Object holding the runner and everything it starts.
//
// Known limit, to verify on a real host: the runner is assigned to the job
// right after it starts, not created suspended, so a child that cmd.exe
// spawns within those first microseconds would be outside the job.
// The job is set to kill its processes when the last handle closes, so the
// tree also dies if the orchestrator itself crashes: no pid is ever killed
// by number later, which rules out hitting an unrelated process.
type procTree struct {
	job windows.Handle
	pid int
}

func afterStart(cmd *exec.Cmd, nice int) procTree {
	t := procTree{pid: cmd.Process.Pid}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return t
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return t
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return t
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return t
	}
	t.job = job
	return t
}

// kill ends every process in the job. Without a job (it could not be
// created) it falls back to taskkill on the runner's tree while it is alive.
func (t procTree) kill(force bool) {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(t.pid)).Run()
}

// killOrphans ends what is left of the tree after the runner exited. Without
// a job nothing is done: the runner's pid may already belong to someone else.
func (t procTree) killOrphans() {
	if t.job != 0 {
		_ = windows.TerminateJobObject(t.job, 1)
	}
}

func (t procTree) close() {
	if t.job != 0 {
		_ = windows.CloseHandle(t.job)
	}
}

// killLeftover never kills by pid on Windows: the Job Object already ended
// the tree when the earlier orchestrator process exited.
func killLeftover(pid int, dir string) bool { return false }
