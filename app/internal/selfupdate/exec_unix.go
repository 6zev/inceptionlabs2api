//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
	"syscall"
)

// execSelf replaces the current process image with exe. On success it never
// returns: the PID, the listening socket's file descriptors and any supervisor
// relationship survive, which is why the gateway can restart itself without
// anyone else's help.
func execSelf(exe string, args []string) error {
	argv := append([]string{exe}, args...)
	if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", exe, err)
	}
	return fmt.Errorf("exec %s returned without an error", exe)
}
