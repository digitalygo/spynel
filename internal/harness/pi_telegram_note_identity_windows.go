//go:build windows

package harness

import "os"

// piTelegramNotePrivateIdentity reports whether Lstat metadata carries the
// private current-user trust identity. Windows os.FileInfo carries no POSIX
// owner or group bits, Windows distribution is unsupported, and the symlink
// and permission repair path stays gated on the Unix contract, so the check
// accepts the entry to keep the shared build compiling without pretending to
// enforce a model the platform cannot express.
func piTelegramNotePrivateIdentity(info os.FileInfo) bool {
	return true
}
