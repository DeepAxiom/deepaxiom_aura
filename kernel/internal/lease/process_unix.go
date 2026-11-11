//go:build !windows

package lease

import (
	"errors"
	"os"
	"syscall"
)

// processExists reports whether a process with this PID exists.
//
// Signal 0 delivers nothing and only performs the existence and permission
// checks. EPERM means the process is there and belongs to someone else, which
// for this purpose is "exists".
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidNamespace identifies the PID namespace this process lives in, so that two
// containers sharing a volume — each with its own PID table — never reason
// about each other's process ids. Empty where the platform has no such thing
// to read; HolderGone then only trusts the check when both sides are empty,
// which is the plain single-machine case.
func pidNamespace() string {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return ns
}
