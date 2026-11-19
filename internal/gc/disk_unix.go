//go:build !windows

package gc

import "syscall"

// diskUsage returns the bytes free to an unprivileged user and the total size of the file system holding path.
func diskUsage(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}
