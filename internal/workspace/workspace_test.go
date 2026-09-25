package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/harness"
	"github.com/digitalygo/spynel/internal/theme"
)

// retiredWorkflowPaths lists retired task/goal workflow locations and the
// retired chat prompt that new and upgraded workspaces must never recreate.
var retiredWorkflowPaths = []string{
	".spynel/tasks", ".spynel/goals",
	".spynel/prompts/chat.md",
	".spynel/prompts/create-task.md", ".spynel/prompts/create-goal.md", ".spynel/prompts/task.md", ".spynel/prompts/goal.md",
	".spynel/prompts/goal-review.md", ".spynel/prompts/review.md", ".spynel/prompts/recovery.md", ".spynel/prompts/heartbeat.md", ".spynel/prompts/notification.md",
	".spynel/instructions/agent-chat.md", ".spynel/instructions/agent-developer.md", ".spynel/instructions/agent-reviewer.md", ".spynel/instructions/agent-notification.md", ".spynel/instructions/agent-heartbeat.md",
}

// retiredDirectories lists retired directories that new and upgraded
// workspaces must never create, inspect, or follow. Existing legacy copies,
// including user-owned symlinks, stay untouched and inert.
var retiredDirectories = []string{".spynel/instructions", ".spynel/runtime/leases"}

func TestInitCreatesDocumentedWorkspace(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		definition, _ := harness.Lookup("claude-code")
		return definition, "/usr/local/bin/claude", true
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		".spynel/config.yaml", ".spynel/AGENTS.md", ".spynel/extensions/README.md",
		".spynel/attachments", ".spynel/history", ".spynel/jobs", ".spynel/runtime", ".spynel/extensions", ".spynel/themes",
		".spynel/themes/spynel.yaml", ".spynel/themes/hack-the-box.yaml", ".spynel/themes/github-colorblind-dark.yaml",
		".spynel/themes/gruvbox-dark.yaml", ".spynel/themes/nord.yaml", ".spynel/themes/okabe-ito-dark.yaml",
		".spynel/themes/gruvbox-light.yaml", ".spynel/themes/rose-pine-dawn.yaml", ".spynel/themes/tol-muted-light.yaml",
		".spynel/themes/catppuccin-latte.yaml",
		".spynel/themes/okabe-ito-light.yaml", ".spynel/themes/solarized-light.yaml",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("missing initialized path %s: %v", path, err)
		}
	}
	for _, path := range retiredWorkflowPaths {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("fresh workspace created retired workflow path %s: %v", path, err)
		}
	}
	for _, path := range retiredDirectories {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("fresh workspace created retired directory %s: %v", path, err)
		}
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root {
		t.Fatalf("config root = %q, want %q", cfg.Root, root)
	}
	if cfg.Harness.Name != "claude-code" {
		t.Fatalf("detected coding harness was not selected: %#v", cfg.Harness)
	}
	if cfg.Harness.Sandbox != "danger-full-access" {
		t.Fatalf("initialized coding harness should be unrestricted: %#v", cfg.Harness)
	}
	if cfg.Channels.TUI.Theme != "spynel" {
		t.Fatalf("initialized TUI theme = %q", cfg.Channels.TUI.Theme)
	}
	themes, err := theme.LoadDir(cfg.StatePath("themes"))
	if err != nil || len(themes) != 12 {
		t.Fatalf("initialized themes = %#v, %v", themes, err)
	}
	for index, builtin := range theme.Builtins() {
		if themes[index].Name != builtin.Name {
			t.Fatalf("initialized theme %d = %q, want %q", index, themes[index].Name, builtin.Name)
		}
		loaded, ok := theme.Find(themes, builtin.Name)
		if !ok || loaded != builtin {
			t.Fatalf("initialized theme %q differs from built-in: loaded=%#v builtin=%#v", builtin.Name, loaded, builtin)
		}
	}
	if !cfg.Speech.Enabled || cfg.Speech.Provider != config.SpeechProviderElevenLabs || cfg.Speech.Language != "en" || cfg.Speech.NumThreads != 2 {
		t.Fatalf("initialized workspace must enable English cloud transcription by default: %#v", cfg.Speech)
	}
	if cfg.Speech.ElevenLabsAPIKey != "" {
		t.Fatalf("initialized workspace stored a speech API key: %q", cfg.Speech.ElevenLabsAPIKey)
	}
	configData, err := os.ReadFile(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "elevenlabs_api_key:") {
		t.Fatalf("initialized workspace config contains a stored speech API key:\n%s", configData)
	}
	configInfo, err := os.Stat(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if configInfo.Mode().Perm() != 0o600 {
		t.Fatalf("initialized config mode = %v, want private 0600", configInfo.Mode().Perm())
	}
	if err := Init(root, false); err == nil {
		t.Fatal("second init should require --force")
	}
}

func TestInitAndUpgradePreserveLegacyInstructionSymlinkWithoutFollowingIt(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })

	for _, operation := range []struct {
		name string
		run  func(string) error
	}{
		{name: "init", run: func(root string) error { return Init(root, false) }},
		{name: "upgrade", run: Upgrade},
	} {
		t.Run(operation.name, func(t *testing.T) {
			root := t.TempDir()
			stateRoot := filepath.Join(root, ".spynel")
			if err := os.Mkdir(stateRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			link := filepath.Join(stateRoot, "instructions")
			if err := os.Symlink(outside, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := operation.run(root); err != nil {
				t.Fatalf("%s must succeed while the retired instructions path is a user-owned symlink: %v", operation.name, err)
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("%s altered the legacy instructions symlink: %v", operation.name, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("%s wrote through the legacy instructions symlink: %#v, %v", operation.name, entries, err)
			}
			if _, err := os.Stat(filepath.Join(stateRoot, "history")); err != nil {
				t.Fatalf("%s did not create the current history directory: %v", operation.name, err)
			}
		})
	}
}

func TestInitAndUpgradeRejectSymlinkedStateRoot(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })

	for _, operation := range []struct {
		name string
		run  func(string) error
	}{
		{name: "init", run: func(root string) error { return Init(root, false) }},
		{name: "upgrade", run: Upgrade},
	} {
		t.Run(operation.name, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			if err := os.Symlink(outside, filepath.Join(root, ".spynel")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := operation.run(root); err == nil || !strings.Contains(err.Error(), "must not be a symbolic link") {
				t.Fatalf("%s error = %v, want rejection of the symlinked state root", operation.name, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("%s wrote through the symlinked state root: %#v, %v", operation.name, entries, err)
			}
		})
	}
}

func TestWorkspaceTemplatesExcludeRepositoryDeveloperPolicy(t *testing.T) {
	generatedRoot := t.TempDir()
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	if err := Init(generatedRoot, false); err != nil {
		t.Fatal(err)
	}

	for _, spec := range files {
		if !strings.HasPrefix(spec.Path, ".spynel/prompts/") && !strings.HasSuffix(spec.Path, "AGENTS.md") {
			continue
		}
		name := strings.TrimPrefix(spec.Template, "templates/")
		embedded, err := Template(name)
		if err != nil {
			t.Fatal(err)
		}
		generated, err := os.ReadFile(filepath.Join(generatedRoot, filepath.FromSlash(spec.Path)))
		if err != nil {
			t.Fatal(err)
		}
		for source, text := range map[string]string{"embedded": string(embedded), "generated": string(generated)} {
			lower := strings.ToLower(text)
			for _, forbidden := range []string{"cache", "gocache", "cold-cache", "shared user cache", "go test", "go build", "build artifact", ".tmp-bin", ".tmp-toolchains", ".tmp-artifacts", ".tmp-tui-captures"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("%s %s contains repository development guidance %q", source, name, forbidden)
				}
			}
		}
	}

}

// fileSnapshot captures the observable identity of one regular file.
type fileSnapshot struct {
	data    []byte
	mode    os.FileMode
	modTime time.Time
}

// snapshotWorkspaceFiles maps every regular file below root/.spynel to its
// bytes, permission mode, and modification time so tests can prove that
// upgrades preserve user files byte-identically without rewriting them.
func snapshotWorkspaceFiles(t *testing.T, root string) map[string]fileSnapshot {
	t.Helper()
	snapshot := map[string]fileSnapshot{}
	stateRoot := filepath.Join(root, ".spynel")
	err := filepath.WalkDir(stateRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(stateRoot, path)
		if err != nil {
			return err
		}
		snapshot[relative] = fileSnapshot{data: data, mode: info.Mode().Perm(), modTime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestUpgradePreservesUserFilesByteIdenticalIncludingRetiredWorkflowData(t *testing.T) {
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	retiredFiles := map[string][]byte{
		filepath.Join(".spynel", "prompts", "chat.md"):                 []byte("user-owned chat prompt override\n"),
		filepath.Join(".spynel", "prompts", "task.md"):                 []byte("custom task prompt\n"),
		filepath.Join(".spynel", "prompts", "create-goal.md"):          []byte("custom goal creation prompt\n"),
		filepath.Join(".spynel", "instructions", "agent-chat.md"):      []byte("Keep this chat preference.\n"),
		filepath.Join(".spynel", "instructions", "agent-heartbeat.md"): []byte("Keep this heartbeat preference.\n"),
		filepath.Join(".spynel", "tasks", "todo", "example.md"):        []byte("---\nid: example\nstatus: todo\n---\n# Example\n"),
		filepath.Join(".spynel", "goals", "active", "outcome.md"):      []byte("---\nid: outcome\nstatus: active\nround: 1\n---\n# Outcome\n"),
		filepath.Join(".spynel", "tasks", "archive", "old.md"):         []byte("---\nid: old\nstatus: done\n---\n# Old\n"),
	}
	for path, data := range retiredFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	summary := filepath.Join(root, ".spynel", "extensions", "README.md")
	summaryCustom := []byte("user-owned extension notes\n")
	if err := os.WriteFile(summary, summaryCustom, 0o600); err != nil {
		t.Fatal(err)
	}
	type dirSnapshot struct {
		mode    os.FileMode
		modTime time.Time
	}
	legacyDirs := map[string]dirSnapshot{}
	for _, relative := range []string{".spynel/instructions", ".spynel/prompts", ".spynel/tasks", ".spynel/goals"} {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		legacyDirs[relative] = dirSnapshot{mode: info.Mode(), modTime: info.ModTime()}
	}
	before := snapshotWorkspaceFiles(t, root)

	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	after := snapshotWorkspaceFiles(t, root)
	for relative, beforeFile := range before {
		afterFile, ok := after[relative]
		if !ok {
			t.Fatalf("upgrade deleted user file %s", relative)
		}
		if string(beforeFile.data) != string(afterFile.data) {
			t.Fatalf("upgrade rewrote user file %s", relative)
		}
		if beforeFile.mode != afterFile.mode {
			t.Fatalf("upgrade changed mode of user file %s: %v != %v", relative, beforeFile.mode, afterFile.mode)
		}
		if !beforeFile.modTime.Equal(afterFile.modTime) {
			t.Fatalf("upgrade rewrote user file %s: modification time changed", relative)
		}
	}
	for relative, beforeDir := range legacyDirs {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("upgrade altered legacy directory %s: %v", relative, err)
		}
		if info.Mode() != beforeDir.mode || !info.ModTime().Equal(beforeDir.modTime) {
			t.Fatalf("upgrade modified legacy directory %s", relative)
		}
	}
	for path, data := range retiredFiles {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != string(data) {
			t.Fatalf("upgrade altered retired user file %s: %q, %v", path, got, err)
		}
	}
	if got, err := os.ReadFile(summary); err != nil || string(got) != string(summaryCustom) {
		t.Fatalf("upgrade overwrote the extension README: %q, %v", got, err)
	}
	// Upgrading must not resurrect retired workflow content beyond what the
	// user already owns.
	for _, path := range retiredWorkflowPaths {
		full := filepath.Join(root, path)
		if _, err := os.Lstat(full); err != nil {
			if !os.IsNotExist(err) {
				t.Fatalf("stat retired path %s: %v", path, err)
			}
			continue
		}
		_, created := retiredFiles[path]
		_, createdDir := map[string]bool{
			filepath.Join(".spynel", "tasks"): true,
			filepath.Join(".spynel", "goals"): true,
		}[path]
		if !created && !createdDir {
			t.Fatalf("upgrade created retired workflow path %s", path)
		}
	}
}

func TestUpgradeDoesNotRecreateRemovedRetiredWorkflowFiles(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	// A workspace that once had retired files and lost them (user deletion or
	// an older layout) must not regain them through upgrade or force init.
	removed := filepath.Join(root, ".spynel", "prompts", "review.md")
	if err := os.MkdirAll(filepath.Dir(removed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removed, []byte("old review prompt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(removed); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(removed); !os.IsNotExist(err) {
		t.Fatalf("upgrade recreated removed retired file: %v", err)
	}
	if err := Init(root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(removed); !os.IsNotExist(err) {
		t.Fatalf("force init recreated removed retired file: %v", err)
	}
	for _, path := range retiredWorkflowPaths {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("re-init created retired workflow path %s: %v", path, err)
		}
	}
	for _, path := range retiredDirectories {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("workspace created retired directory %s: %v", path, err)
		}
	}
}

func TestUpgradeRestoresMissingCurrentAssetsOnly(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(".spynel", "extensions", "README.md"),
		filepath.Join(".spynel", "AGENTS.md"),
	} {
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	// The generic runtime root must come back even when a partial layout
	// lost it entirely; the retired lease directory inside it must not.
	runtimeRoot := filepath.Join(root, ".spynel", "runtime")
	if err := os.Remove(runtimeRoot); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(".spynel", "extensions", "README.md"),
		filepath.Join(".spynel", "AGENTS.md"),
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("upgrade did not restore missing current asset %s: %v", path, err)
		}
	}
	info, err := os.Stat(runtimeRoot)
	if err != nil {
		t.Fatalf("upgrade did not restore the current runtime root: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("restored runtime root is not a directory: %v", info.Mode())
	}
	for _, path := range retiredWorkflowPaths {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("upgrade created retired workflow path %s: %v", path, err)
		}
	}
	for _, path := range retiredDirectories {
		if _, err := os.Lstat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("upgrade created retired directory %s: %v", path, err)
		}
	}
}

func TestTemplateConfigOmitsRetiredWorkflowSettings(t *testing.T) {
	data, err := Template("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, retired := range []string{
		"orchestrator:", "reviews:", "chat_agent_prefix", "developer_agent_prefix",
		"reviewer_agent_prefix", "heartbeat_agent_prefix", "semantic_heartbeat",
		"task_notifications", "max_parallel",
	} {
		if strings.Contains(text, retired) {
			t.Errorf("workspace template config still contains retired setting %q:\n%s", retired, text)
		}
	}
	for _, required := range []string{
		"sandbox: danger-full-access", "provider: elevenlabs", "theme: spynel",
		"directory: .spynel/extensions", "history_max_messages: 50", "attachment_max_mb: 100",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("workspace template config lost current default %q:\n%s", required, text)
		}
	}
	path := filepath.Join(t.TempDir(), config.FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("template config must validate: %v", err)
	}
}

func TestUpgradePreservesCustomThemeCollection(t *testing.T) {
	root := t.TempDir()
	themeDir := filepath.Join(root, ".spynel", "themes")
	if err := os.MkdirAll(themeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(themeDir, "custom.yaml")
	custom := []byte("user-owned custom theme\n")
	if err := os.WriteFile(customPath, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(themeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "custom.yaml" {
		t.Fatalf("upgrade changed custom theme collection: %#v", entries)
	}
	if got, err := os.ReadFile(customPath); err != nil || string(got) != string(custom) {
		t.Fatalf("upgrade changed custom theme: %q, %v", got, err)
	}
}

func TestForceInitAddsRevisedThemesWithoutReplacingExistingFiles(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(root, ".spynel", "themes", "spynel.yaml")
	custom := []byte("user-owned theme file\n")
	if err := os.WriteFile(customPath, custom, 0o600); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(root, ".spynel", "themes", "tol-muted-light.yaml")
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	customExtraPath := filepath.Join(root, ".spynel", "themes", "tokyo-night.yaml")
	if err := os.WriteFile(customExtraPath, []byte("user-owned custom theme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Init(root, true); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(customPath); err != nil || string(got) != string(custom) {
		t.Fatalf("force init replaced existing stock-named file: %q, %v", got, err)
	}
	if got, err := os.ReadFile(customExtraPath); err != nil || string(got) != "user-owned custom theme\n" {
		t.Fatalf("force init removed custom theme: %q, %v", got, err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("force init did not materialize missing revised theme: %v", err)
	}
}

func TestInitLeavesHarnessSelectionOpenWhenNothingIsDetected(t *testing.T) {
	previousDetection := detectCodingHarness
	detectCodingHarness = func(func(string) (string, error)) (harness.Definition, string, bool) {
		return harness.Definition{}, "", false
	}
	t.Cleanup(func() { detectCodingHarness = previousDetection })
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(config.PathForRoot(root))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Name != "" {
		t.Fatalf("unexpected coding harness selection: %#v", cfg.Harness)
	}
}

func TestUpgradePreservesConfigIncludingUnusedKeys(t *testing.T) {
	root := t.TempDir()
	if err := Init(root, false); err != nil {
		t.Fatal(err)
	}
	path := config.PathForRoot(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "\n    tui:\n", "\n    tui:\n        enabled: false\n", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upgrade(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(data) {
		t.Fatal("upgrade rewrote user configuration")
	}
}
