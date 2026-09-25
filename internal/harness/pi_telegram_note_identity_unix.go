//go:build unix

package harness

import (
	"os"
	"syscall"
)

// piTelegramNotePrivateIdentity reports whether Lstat metadata describes an
// entry owned by the current effective user with no group or other
// permission bits. It enforces the private runtime trust contract for the
// Telegram note extension on every Unix platform Spynel releases.
func piTelegramNotePrivateIdentity(info os.FileInfo) bool {
	if info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
