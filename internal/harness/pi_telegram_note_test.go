package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// noteExtensionTestConfig returns a config whose runtime directory is the
// disposable root itself, mirroring the SessionsFile-backed production shape.
func noteExtensionTestConfig(root string) HarnessConfig {
	return HarnessConfig{Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")}
}

// noteExtensionPrivateRoot returns a disposable 0700 directory. t.TempDir()
// creates its per-call subdirectory with the umask-derived 0777&^umask mode
// on current Go releases, so tests that use the root itself as the private
// runtime directory need an explicitly private root like production
// workspaces have.
func noteExtensionPrivateRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "spynel-note-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private test root %q is not a private directory: %v", root, info.Mode())
	}
	return root
}

// noteExtensionWorkspaceConfig returns a config using the canonical
// .spynel/workspace layout under root so state-root trust is exercised.
func noteExtensionWorkspaceConfig(root string) HarnessConfig {
	return HarnessConfig{
		Cwd:          root,
		SessionsFile: filepath.Join(root, ".spynel", "runtime", "sessions.json"),
	}
}

// noteExtensionSharedSentinel prepares an outside directory holding one
// sentinel file so tests can prove no write ever travels through a symlink.
func noteExtensionSharedSentinel(t *testing.T) (shared, sentinel string) {
	t.Helper()
	shared = filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel = filepath.Join(shared, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("sentinel that must never be overwritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	return shared, sentinel
}

// assertNoteExtensionSharedSentinelUnchanged proves the outside directory
// still holds exactly the untouched sentinel file and nothing else.
func assertNoteExtensionSharedSentinelUnchanged(t *testing.T, shared, sentinel string) {
	t.Helper()
	content, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(content, []byte("sentinel that must never be overwritten")) {
		t.Fatalf("sentinel %q was modified: %q", sentinel, content)
	}
	entries, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel.txt" {
		t.Fatalf("shared directory %q gained entries through the symlink: %v", shared, entries)
	}
}

func TestEnsurePiTelegramNoteExtensionMaterializesPrivateFile(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	path, err := ensurePiTelegramNoteExtension(noteExtensionTestConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("materialized path %q is not absolute", path)
	}
	assertTelegramNoteExtensionFile(t, path)
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("runtime directory %q is not a directory", filepath.Dir(path))
	}
	// The embedded source must be a dependency-free additive extension: it
	// never returns a forced prompt, registers tools or commands, or replaces
	// the prompt the user's Pi resources built.
	source := string(piTelegramNoteExtensionSource)
	for _, forbidden := range []string{"forceSystemPrompt", "systemPrompt =", "registerTool", "registerCommand", "import "} {
		if bytes.Contains([]byte(source), []byte(forbidden)) {
			t.Fatalf("embedded Telegram note extension contains forbidden %q construct", forbidden)
		}
	}
	for _, required := range []string{`"before_agent_start"`, "systemPromptOptions.sections." + piTelegramNoteSectionName, "export default"} {
		if !bytes.Contains([]byte(source), []byte(required)) {
			t.Fatalf("embedded Telegram note extension lacks required %q construct", required)
		}
	}
}

func TestEnsurePiTelegramNoteExtensionReusesCurrentFile(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	first, err := ensurePiTelegramNoteExtension(noteExtensionTestConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensurePiTelegramNoteExtension(noteExtensionTestConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("ensure returned %q then %q", first, second)
	}
	infoAfter, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatal("ensure rewrote an already current extension file")
	}
}

func TestEnsurePiTelegramNoteExtensionReplacesSymlinkWithoutFollowingIt(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	outside := filepath.Join(root, "outside-sentinel.md")
	sentinel := []byte("sentinel that must never be overwritten")
	if err := os.WriteFile(outside, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := noteExtensionTestConfig(root)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	path, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if path != target {
		t.Fatalf("ensure returned %q, want %q", path, target)
	}
	assertTelegramNoteExtensionFile(t, path)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("symlink at %q survived materialization", path)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, sentinel) {
		t.Fatalf("symlink target was overwritten: %q", content)
	}
}

func TestEnsurePiTelegramNoteExtensionReplacesUnexpectedContent(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	cfg := noteExtensionTestConfig(root)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("// tampered or stale extension"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertTelegramNoteExtensionFile(t, path)
}

func TestEnsurePiTelegramNoteExtensionRepairsLoosePermissions(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	cfg := noteExtensionTestConfig(root)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, piTelegramNoteExtensionSource, 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertTelegramNoteExtensionFile(t, path)
}

func TestEnsurePiTelegramNoteExtensionRefusesSymlinkedRuntimeDirectory(t *testing.T) {
	home := t.TempDir()
	shared, sentinel := noteExtensionSharedSentinel(t)
	cfg := noteExtensionWorkspaceConfig(home)
	if err := os.Mkdir(filepath.Join(home, ".spynel"), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Dir(target)); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensure accepted a symlinked runtime directory that must fail closed")
	}
	assertNoteExtensionSharedSentinelUnchanged(t, shared, sentinel)
	info, err := os.Lstat(filepath.Dir(target))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("runtime symlink survived in unexpected state: %v %v", info, err)
	}
}

func TestEnsurePiTelegramNoteExtensionRefusesSymlinkedStateRoot(t *testing.T) {
	home := t.TempDir()
	shared, sentinel := noteExtensionSharedSentinel(t)
	cfg := noteExtensionWorkspaceConfig(home)
	if err := os.Symlink(shared, filepath.Join(home, ".spynel")); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensure accepted a symlinked .spynel state root that must fail closed")
	}
	assertNoteExtensionSharedSentinelUnchanged(t, shared, sentinel)
}

func TestEnsurePiTelegramNoteExtensionRefusesLooseRuntimeModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o770, 0o755, 0o751, 0o705} {
		t.Run(mode.String(), func(t *testing.T) {
			home := t.TempDir()
			cfg := noteExtensionWorkspaceConfig(home)
			target, err := piTelegramNoteExtensionPath(cfg)
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := filepath.Dir(target)
			if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(runtimeDir, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
				t.Fatalf("ensure accepted runtime mode %v that must fail closed", mode)
			}
			info, err := os.Lstat(runtimeDir)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode().Perm() != mode {
				t.Fatalf("ensure rewrote the loose runtime directory: %v", info.Mode())
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("extension file appeared despite the refused runtime directory: %v", err)
			}
		})
	}
}

func TestEnsurePiTelegramNoteExtensionRefusesLooseStateRoot(t *testing.T) {
	home := t.TempDir()
	cfg := noteExtensionWorkspaceConfig(home)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Dir(filepath.Dir(target))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateRoot, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensure accepted a loose .spynel state root that must fail closed")
	}
	info, err := os.Lstat(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o770 {
		t.Fatalf("ensure rewrote the loose state root: %v", info.Mode())
	}
}

func TestEnsurePiTelegramNoteExtensionCreatesPrivateStateRootAndRestarts(t *testing.T) {
	home := t.TempDir()
	cfg := noteExtensionWorkspaceConfig(home)
	first, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertTelegramNoteExtensionFile(t, first)
	for _, directory := range []string{filepath.Join(home, ".spynel"), filepath.Dir(first)} {
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("created trust directory %q is not a real directory: %v", directory, info.Mode())
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("created trust directory %q is not private: %v", directory, info.Mode().Perm())
		}
	}
	infoBefore, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("restart returned %q, want %q", second, first)
	}
	infoAfter, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatal("restart rewrote an already current extension file")
	}
}

func TestEnsurePiTelegramNoteExtensionRefusesForeignOwnedEntries(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("rewriting entry ownership requires root; skipping the foreign-identity trust test")
	}
	foreignUID := os.Geteuid() + 1
	home := t.TempDir()
	cfg := noteExtensionWorkspaceConfig(home)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Dir(target)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A private 0700 runtime directory owned by another user must fail closed
	// even though this process could read it.
	if err := os.Chown(runtimeDir, foreignUID, -1); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensure accepted a foreign-owned runtime directory")
	}
	info, err := os.Lstat(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := noteFileOwnerUID(info); !ok || uid != uint32(foreignUID) {
		t.Fatalf("ensure rewrote the foreign-owned runtime directory: %v", info)
	}
	if err := os.Chown(runtimeDir, os.Geteuid(), -1); err != nil {
		t.Fatal(err)
	}
	// A foreign-owned regular file with exact embedded bytes and 0600 inside
	// the trusted private runtime is repaired into current-user ownership.
	if err := os.WriteFile(target, piTelegramNoteExtensionSource, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(target, foreignUID, -1); err != nil {
		t.Fatal(err)
	}
	path, err := ensurePiTelegramNoteExtension(cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertTelegramNoteExtensionFile(t, path)
	fileInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := noteFileOwnerUID(fileInfo); !ok || uid != uint32(os.Geteuid()) {
		t.Fatalf("repaired extension file %q kept foreign ownership: %v", path, fileInfo)
	}
}

func TestEnsurePiTelegramNoteExtensionRefusesDirectoryTarget(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	cfg := noteExtensionTestConfig(root)
	target, err := piTelegramNoteExtensionPath(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePiTelegramNoteExtension(cfg); err == nil {
		t.Fatal("ensure succeeded over a directory target that must be refused")
	}
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory target %q was removed or replaced: %v", target, err)
	}
}

func TestEnsurePiTelegramNoteExtensionConcurrentLaunches(t *testing.T) {
	root := noteExtensionPrivateRoot(t)
	cfg := noteExtensionTestConfig(root)
	const launches = 8
	paths := make([]string, launches)
	errs := make([]error, launches)
	var wg sync.WaitGroup
	for index := range launches {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			paths[slot], errs[slot] = ensurePiTelegramNoteExtension(cfg)
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent launch %d failed: %v", index, err)
		}
		if paths[index] != paths[0] {
			t.Fatalf("concurrent launch %d returned %q, want %q", index, paths[index], paths[0])
		}
	}
	assertTelegramNoteExtensionFile(t, paths[0])
	// A repair racing an already current file still converges on the exact
	// embedded bytes.
	if err := os.WriteFile(paths[0], []byte("// raced tamper"), 0o600); err != nil {
		t.Fatal(err)
	}
	var repairWG sync.WaitGroup
	for range launches {
		repairWG.Add(1)
		go func() {
			defer repairWG.Done()
			if _, err := ensurePiTelegramNoteExtension(cfg); err != nil {
				t.Errorf("concurrent repair failed: %v", err)
			}
		}()
	}
	repairWG.Wait()
	assertTelegramNoteExtensionFile(t, paths[0])
}

func TestPiEphemeralDiscoveryLaunchesCarryNoTelegramExtension(t *testing.T) {
	command, root, logPath := portableHarnessFixture(t, "pi-model-capabilities")
	pi, err := NewPi(HarnessConfig{Command: command, Cwd: root, SessionsFile: filepath.Join(root, "sessions.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pi.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pi.Close() }()
	if _, err := pi.Models(ctx); err != nil {
		t.Fatal(err)
	}
	discoveries := 0
	for _, record := range launchInvocations(t, logPath) {
		if containsArgument(record.Args, "--no-session") {
			discoveries++
		}
		if count := countArgument(record.Args, "--extension"); count != 0 {
			t.Fatalf("discovery launch argv carried %d --extension flags: %q", count, record.Args)
		}
		if count := countArgument(record.Args, "--append-system-prompt"); count != 0 {
			t.Fatalf("discovery launch argv carried %d --append-system-prompt flags: %q", count, record.Args)
		}
	}
	if discoveries == 0 {
		t.Fatal("fixture produced no ephemeral discovery launches")
	}
	if _, err := os.Stat(filepath.Join(root, piTelegramNoteExtensionFileName)); !os.IsNotExist(err) {
		t.Fatalf("discovery materialized the Telegram note extension: %v", err)
	}
}
