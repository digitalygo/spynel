package updater

import "path/filepath"

// NPMPackageName is the published npm package identity. npm installs it in the
// scoped global layout <prefix>/lib/node_modules/@digitalygo/spynel.
const NPMPackageName = "@digitalygo/spynel"

// npmPackagePrefix validates the scoped global npm package layout and returns
// the installation prefix npm itself should target.
func npmPackagePrefix(root string) (string, bool) {
	root = filepath.Clean(root)
	if filepath.Base(root) != "spynel" || filepath.Base(filepath.Dir(root)) != "@digitalygo" {
		return "", false
	}
	modules := filepath.Dir(filepath.Dir(root))
	if filepath.Base(modules) != "node_modules" || filepath.Base(filepath.Dir(modules)) != "lib" {
		return "", false
	}
	return filepath.Dir(filepath.Dir(modules)), true
}

// NPMPackageRoot returns the global npm package root below a modules directory.
func NPMPackageRoot(modules string) string {
	return filepath.Join(modules, filepath.FromSlash(NPMPackageName))
}
