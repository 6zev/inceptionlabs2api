//go:build windows

package selfupdate

import "errors"

// execSelf is unreachable: Update refuses to run on Windows because no atomic
// replace of a running .exe exists there.
func execSelf(string, []string) error {
	return errors.New("self-update is not supported on Windows")
}
