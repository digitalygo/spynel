//go:build windows

package harness

import "os"

// noteFileOwnerUID reports the owning user ID of one Lstat result. Windows
// os.FileInfo carries no POSIX owner, so tests rely on the false identity.
func noteFileOwnerUID(info os.FileInfo) (uint32, bool) {
	return 0, false
}
