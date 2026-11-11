//go:build windows

package lease

import "syscall"

// processExists reports whether a process with this PID exists and has not
// exited.
//
// Opening it is not enough: a handle to a process that has already exited can
// still be opened for as long as anyone else holds one, so the exit code is
// what decides. Any failure to find out answers "exists" — the safe direction,
// since a wrong "gone" is the one mistake that would displace a live leader.
func processExists(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	const stillActive = 259
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER is what Windows answers for a PID that names
		// no process. Anything else (access denied, most often) means there is
		// one.
		return err != syscall.Errno(87)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

// pidNamespace is empty on Windows: there is one process table per machine.
func pidNamespace() string { return "" }
