//go:build unix

package harness

import (
	"os"
	"syscall"
)

// noteFileOwnerUID reports the owning user ID of one Lstat result so tests
// can assert the current-user trust identity.
func noteFileOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}
