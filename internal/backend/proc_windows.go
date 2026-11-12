//go:build windows

package backend

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
func setProcAttrs(cmd *exec.Cmd, nice int, ra *runAsAccount) {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if nice > 0 {
		flags |= belowNormalPriorityClass
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if ra != nil {
		cmd.SysProcAttr.Token = syscall.Token(ra.token)
	}
}

// runAsAccount is a logon token of a built-in service account (native.run_as)
// that every job runs under. Getting it needs LocalSystem (SeTcbPrivilege).
type runAsAccount struct {
	token   windows.Token
	sid     string // e.g. S-1-5-20
	user    string
	profile string
}

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procLogonUserW            = advapi32.NewProc("LogonUserW")
	procCreateRestrictedToken = advapi32.NewProc("CreateRestrictedToken")
)

const (
	logon32LogonService    = 5
	logon32ProviderDefault = 0
	disableMaxPrivilege    = 0x1
)

// restrict returns a copy of tok with every privilege but SeChangeNotify
// removed. Service accounts hold SeImpersonate and SeAssignPrimaryToken, the
// usual way from NETWORK SERVICE to SYSTEM; a job must not keep them.
func restrict(tok windows.Token) (windows.Token, error) {
	var out windows.Token
	r, _, e := procCreateRestrictedToken.Call(uintptr(tok), disableMaxPrivilege, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return 0, e
	}
	return out, nil
}

func openRunAs(name string) (*runAsAccount, error) {
	domain, user, ok := strings.Cut(name, `\`)
	if !ok {
		return nil, fmt.Errorf("native.run_as %q: want DOMAIN\\USER", name)
	}
	u, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return nil, err
	}
	d, err := windows.UTF16PtrFromString(domain)
	if err != nil {
		return nil, err
	}
	var full windows.Token
	r, _, e := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), 0,
		logon32LogonService, logon32ProviderDefault, uintptr(unsafe.Pointer(&full)))
	if r == 0 {
		return nil, fmt.Errorf("native.run_as %q: LogonUser (the service must run as LocalSystem): %w", name, e)
	}
	tok, err := restrict(full)
	full.Close()
	if err != nil {
		return nil, fmt.Errorf("native.run_as %q: restrict token: %w", name, err)
	}
	tu, err := tok.GetTokenUser()
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("native.run_as %q: token user: %w", name, err)
	}
	dir, err := tok.GetUserProfileDirectory()
	if err != nil {
		tok.Close()
		return nil, fmt.Errorf("native.run_as %q: profile dir: %w", name, err)
	}
	return &runAsAccount{token: tok, sid: tu.User.Sid.String(), user: user, profile: dir}, nil
}

// env points the job's profile and temp variables at the account's own
// profile instead of LocalSystem's. native.env still wins over these.
func (ra *runAsAccount) env() map[string]string {
	tmp := filepath.Join(ra.profile, "AppData", "Local", "Temp")
	return map[string]string{
		"USERPROFILE":  ra.profile,
		"APPDATA":      filepath.Join(ra.profile, "AppData", "Roaming"),
		"LOCALAPPDATA": filepath.Join(ra.profile, "AppData", "Local"),
		"TEMP":         tmp,
		"TMP":          tmp,
	}
}

// grantJobsDir first resets native.root (the parent of dir) to SYSTEM and
// Administrators only, so the job account cannot touch the shared runner
// release or the logs whatever the root inherited. Then it lets the account
// modify the jobs dir (inherited by every job dir). The runner also insists on
// listing every directory above its own, so each ancestor of the root gets
// read/list for that directory only: no inheritance, so files there (the
// credential) keep their own ACL.
func (ra *runAsAccount) grantJobsDir(dir string) error {
	icacls := func(args ...string) error {
		out, err := exec.Command("icacls", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("icacls %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	root := filepath.Dir(dir)
	if err := icacls(root, "/inheritance:r", "/grant:r", "*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F"); err != nil {
		return err
	}
	// Explicit entries an earlier setup may have left on the root: the job
	// account, Users, Authenticated Users, Everyone.
	if err := icacls(root, "/remove:g", "*"+ra.sid, "*S-1-5-32-545", "*S-1-5-11", "*S-1-1-0"); err != nil {
		return err
	}
	// Children with explicit entries of their own fall back to the root's ACL.
	for _, sub := range []string{"dist", "logs"} {
		if p := filepath.Join(root, sub); dirExists(p) {
			if err := icacls(p, "/reset", "/T", "/C", "/Q"); err != nil {
				return err
			}
		}
	}
	if err := icacls(root, "/grant", "*"+ra.sid+":(RX)"); err != nil {
		return err
	}
	// The jobs dir itself is read-only for the account (no deleting it or its
	// siblings' parents); what is created inside it is the account's to modify.
	if err := icacls(dir, "/remove:g", "*"+ra.sid); err != nil {
		return err
	}
	if err := icacls(dir, "/grant", "*"+ra.sid+":(RX)", "*"+ra.sid+":(OI)(CI)(IO)M"); err != nil {
		return err
	}
	for p := filepath.Dir(root); ; p = filepath.Dir(p) {
		if p == filepath.Dir(p) { // volume root
			return nil
		}
		if err := icacls(p, "/grant", "*"+ra.sid+":(RX)"); err != nil {
			return err
		}
	}
}

func (ra *runAsAccount) close() { ra.token.Close() }

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

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
