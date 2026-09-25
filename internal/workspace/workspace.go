package workspace

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitalygo/spynel/internal/config"
	"github.com/digitalygo/spynel/internal/fsx"
	"github.com/digitalygo/spynel/internal/harness"
	"github.com/digitalygo/spynel/internal/theme"
)

//go:embed templates/*.md templates/*.yaml
var templates embed.FS

var detectCodingHarness = harness.Detect

type fileSpec struct {
	Path       string
	Template   string
	Executable bool
}

// files lists every workspace file materialized from the embedded templates.
// The retired task/goal workflow prompts, the retired chat prompt, workflow
// AGENTS contracts, and persistent role instructions are intentionally
// absent: new and upgraded workspaces never recreate them, while existing
// user-owned copies stay untouched and inert.
var files = []fileSpec{
	{Path: config.FileName, Template: "templates/config.yaml"},
	{Path: ".spynel/AGENTS.md", Template: "templates/workspace-AGENTS.md"},
	{Path: ".spynel/extensions/README.md", Template: "templates/extensions.md"},
}

// directories lists the current runtime directories created by Init and
// Upgrade. `.spynel/runtime` itself stays the generic runtime root for
// instance election state, logs, the outbox, and the cleanup lock. The
// retired `.spynel/instructions` and `.spynel/runtime/leases` directories
// are intentionally absent: they are never created, validated, inspected,
// or followed, while existing legacy copies stay untouched and inert.
var directories = []string{
	".spynel/history", ".spynel/jobs", ".spynel/attachments", ".spynel/runtime", ".spynel/extensions", ".spynel/themes",
}

func ensureRealDirectory(path, description string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.Mkdir(path, 0o700); err == nil {
			return nil
		} else if !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symbolic link", description)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory", description)
	}
	return nil
}

func Init(root string, force bool) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	if err := ensureRealDirectory(filepath.Join(abs, ".spynel"), ".spynel state path"); err != nil {
		return err
	}
	configPath := config.PathForRoot(abs)
	if !force {
		if _, err := os.Stat(configPath); err == nil {
			return errors.New(".spynel/config.yaml already exists (use --force to restore missing templates)")
		}
	}
	for _, dir := range directories {
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	if err := theme.InstallBuiltins(filepath.Join(abs, ".spynel", "themes")); err != nil {
		return err
	}
	createdConfig := false
	for _, spec := range files {
		target := filepath.Join(abs, filepath.FromSlash(spec.Path))
		if force && spec.Path == config.FileName {
			if _, err := os.Stat(target); err == nil {
				continue
			}
		}
		if _, err := os.Stat(target); err == nil {
			continue
		}
		data, err := templates.ReadFile(spec.Template)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{PROJECT_ROOT}}", filepath.ToSlash(abs)))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if spec.Executable {
			mode = 0o700
		}
		if err := fsx.AtomicCreateFile(target, data, mode); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		if spec.Path == config.FileName {
			createdConfig = true
		}
	}
	if createdConfig {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		if detected, _, ok := detectCodingHarness(nil); ok {
			cfg.Harness.Name = detected.Name
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	return nil
}

// Upgrade restores missing runtime directories and embedded support files. It
// preserves current configuration and user-owned prompts, instructions,
// themes, and extensions. Retired task/goal workflow files are never
// recreated, the retired instructions and lease directories are never
// created, inspected, or followed, and unused configuration keys disappear
// only on the next save.
func Upgrade(root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	if err := ensureRealDirectory(filepath.Join(abs, ".spynel"), ".spynel state path"); err != nil {
		return err
	}
	for _, dir := range directories {
		if err := os.MkdirAll(filepath.Join(abs, filepath.FromSlash(dir)), 0o700); err != nil {
			return err
		}
	}
	for _, spec := range files {
		if spec.Path == config.FileName {
			continue
		}
		target := filepath.Join(abs, filepath.FromSlash(spec.Path))
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		data, err := templates.ReadFile(spec.Template)
		if err != nil {
			return err
		}
		data = []byte(strings.ReplaceAll(string(data), "{{PROJECT_ROOT}}", filepath.ToSlash(abs)))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if spec.Executable {
			mode = 0o700
		}
		if err := fsx.AtomicCreateFile(target, data, mode); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func Template(name string) ([]byte, error) {
	data, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("embedded template %s: %w", name, err)
	}
	return data, nil
}
