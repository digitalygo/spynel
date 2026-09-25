package harness

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The Telegram note extension is one dependency-free Pi extension embedded
// in the Spynel binary and materialized into the private workspace runtime
// directory for chat:telegram: Pi RPC launches only. Pi receives it through
// one additive --extension argument, so the user's ordinary Pi resource
// discovery, including global and trusted-project APPEND_SYSTEM.md, stays
// active and no other channel, session key, or discovery process loads it.
//
//go:embed pi_telegram_note.js
var piTelegramNoteExtensionSource []byte

const (
	// piTelegramNoteSectionName is the one namespaced system prompt section
	// the extension writes on before_agent_start. Pi wraps it as
	// <spynel_telegram> in the structured system prompt.
	piTelegramNoteSectionName = "spynel_telegram"

	// piTelegramNoteExtensionFileName is the materialized file name inside
	// the workspace runtime directory.
	piTelegramNoteExtensionFileName = "spynel-telegram-note.js"
)

// piRuntimeDirectory is the workspace runtime directory holding Spynel's
// harness runtime state. It mirrors the Pi session directory derivation so
// the extension file lands beside the persisted session map under the fixed
// .spynel/runtime root.
func piRuntimeDirectory(cfg HarnessConfig) string {
	if cfg.SessionsFile == "" {
		return filepath.Join(cfg.Cwd, ".spynel", "runtime")
	}
	return filepath.Dir(cfg.SessionsFile)
}

// piTelegramNoteExtensionPath returns the absolute private runtime path
// where the Telegram note extension is materialized for cfg.
func piTelegramNoteExtensionPath(cfg HarnessConfig) (string, error) {
	runtimeDir, err := filepath.Abs(piRuntimeDirectory(cfg))
	if err != nil {
		return "", fmt.Errorf("resolve the Spynel runtime directory: %w", err)
	}
	return filepath.Join(runtimeDir, piTelegramNoteExtensionFileName), nil
}

// ensurePiTelegramNoteExtension materializes the embedded Telegram note
// extension inside the workspace runtime directory and returns its absolute
// path. The .spynel state root and the runtime directory beneath it are
// trusted before any file work: each is validated by Lstat as a real
// current-user-owned directory with no group or other permission bits, a
// missing directory is created 0700, and a pre-existing directory that
// violates that private trust contract fails the launch closed instead of
// being chmodded or replaced, so a symlinked, shared, or foreign-owned
// runtime can never redirect or rewrite the executable extension. An
// existing target is reused only when it is a regular non-symlink file with
// exactly the embedded bytes, private permissions, and current-user
// ownership; a symlink, another non-regular entry, unexpected content, loose
// permissions, or foreign ownership are repaired through a 0600 temporary
// file and one atomic rename that never follows the target, and the
// published file is revalidated afterwards. Concurrent launches materialize
// identical bytes, so overlapping repairs stay safe without a lock. A
// directory occupying the target path is never deleted and fails the launch
// instead.
func ensurePiTelegramNoteExtension(cfg HarnessConfig) (string, error) {
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		return "", err
	}
	runtimeDir := filepath.Dir(target)
	// The state root is trusted before anything beneath it is created or
	// read: a symlinked or shared .spynel directory could redirect the whole
	// runtime tree to attacker-writable storage.
	if stateRoot := filepath.Dir(runtimeDir); filepath.Base(stateRoot) == ".spynel" {
		if err := ensurePiTelegramNoteDirectory(stateRoot); err != nil {
			return "", err
		}
	}
	if err := ensurePiTelegramNoteDirectory(runtimeDir); err != nil {
		return "", err
	}
	// Rename would silently replace an empty directory target, so the trust
	// contract refuses any directory occupying the extension path up front.
	if info, err := os.Lstat(target); err == nil && info.IsDir() {
		return "", fmt.Errorf("the Telegram note extension path %q is occupied by a directory", target)
	}
	if piTelegramNoteExtensionCurrent(target) {
		return target, nil
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), "."+piTelegramNoteExtensionFileName+".*")
	if err != nil {
		return "", fmt.Errorf("prepare the Telegram note extension file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporary.Write(piTelegramNoteExtensionSource); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write the Telegram note extension file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("flush the Telegram note extension file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close the Telegram note extension file: %w", err)
	}
	if err := os.Chmod(temporaryName, 0o600); err != nil {
		return "", fmt.Errorf("restrict the Telegram note extension file: %w", err)
	}
	// The rename never follows or writes through the target: it replaces a
	// symlink or regular file atomically and refuses a directory target.
	if err := os.Rename(temporaryName, target); err != nil {
		return "", fmt.Errorf("publish the Telegram note extension file %q: %w", target, err)
	}
	if !piTelegramNoteExtensionCurrent(target) {
		return "", fmt.Errorf("the Telegram note extension file at %q is not the expected private regular file", target)
	}
	return target, nil
}

// ensurePiTelegramNoteDirectory validates one trust-boundary directory on
// the extension path. A missing directory is created 0700; an existing entry
// must be a real directory (Lstat never follows the final path element, so a
// symlink never qualifies), owned by the current effective user, and carry
// no group or other permission bits. An existing directory that violates the
// private trust contract fails closed instead of being chmodded or replaced.
func ensurePiTelegramNoteDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
			return fmt.Errorf("create the Spynel directory %q: %w", path, mkdirErr)
		}
		// Re-inspect whatever the Mkdir race may have created before trusting it.
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect the Spynel directory %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the Spynel directory %q is not a real directory", path)
	}
	if !piTelegramNotePrivateIdentity(info) {
		return fmt.Errorf("the Spynel directory %q is not private and owned by the current user", path)
	}
	return nil
}

// piTelegramNoteExtensionCurrent reports whether path currently holds the
// embedded extension as a private regular file owned by the current user.
// Symlinks and other non-regular entries never qualify because Lstat does
// not follow them, and content, permission, or ownership mismatches force a
// repair.
func piTelegramNoteExtensionCurrent(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if !piTelegramNotePrivateIdentity(info) {
		return false
	}
	content, err := os.ReadFile(path)
	return err == nil && bytes.Equal(content, piTelegramNoteExtensionSource)
}
