package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// bunTargets are bun's names for pb's platforms: the compiled
// executable for each is bun's runtime for that platform with the
// bundled program embedded.
var bunTargets = map[string]string{
	"linux/amd64":   "bun-linux-x64",
	"linux/arm64":   "bun-linux-arm64",
	"darwin/amd64":  "bun-darwin-x64",
	"darwin/arm64":  "bun-darwin-arm64",
	"windows/amd64": "bun-windows-x64",
	"windows/arm64": "bun-windows-arm64",
}

// buildNode installs the npm package at the version into a throwaway
// package, no lifecycle script run, reads the executable's script
// from the package's `bin`, and compiles it with bun into a standalone
// executable per platform, every dependency bundled.
func buildNode(ctx context.Context, p *catalog.Plugin, version string, platforms []string, out string) error {
	tmp, err := os.MkdirTemp("", "pb-plugins-node-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	manifest := map[string]any{
		"name": "tree", "private": true,
		"dependencies": map[string]string{p.Package: strings.TrimPrefix(version, "v")},
	}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(tmp, "package.json"), raw, 0o644); err != nil {
		return err
	}
	if err := run(ctx, tmp, nil, "bun", "install", "--no-progress", "--ignore-scripts"); err != nil {
		return err
	}
	pkgDir := filepath.Join(tmp, "node_modules", filepath.FromSlash(p.Package))
	pkg, err := readPackage(pkgDir)
	if err != nil {
		return err
	}
	script, err := binScript(pkg, pkgDir, p.Entrypoint)
	if err != nil {
		return err
	}
	script, err = bundleEntry(script, pkg.Type)
	if err != nil {
		return err
	}
	for _, pl := range platforms {
		outfile := filepath.Join(TreeDir(out, pl), p.Entrypoint)
		if err := run(ctx, tmp, nil, "bun", "build", "--compile", "--target="+bunTargets[pl], "--outfile", outfile, script); err != nil {
			return err
		}
		// bun names a windows executable `.exe` whatever the outfile
		// says; the tree's entrypoint is the bare name on every
		// platform.
		if _, err := os.Stat(outfile); err != nil {
			if err := os.Rename(outfile+".exe", outfile); err != nil {
				return fmt.Errorf("%s: bun wrote neither %s nor %s.exe", pl, outfile, outfile)
			}
		}
	}
	return nil
}

// npmPackage is what the catalog reads of a package.json: its bin
// and its module type.
type npmPackage struct {
	Bin  json.RawMessage `json:"bin"`
	Type string          `json:"type"`
}

// readPackage reads the package's package.json.
func readPackage(pkgDir string) (*npmPackage, error) {
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return nil, err
	}
	var manifest npmPackage
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("%s/package.json: %w", pkgDir, err)
	}
	return &manifest, nil
}

// binScript is the path of the package's executable named in its
// `bin`: the map's entry of the name, or the one string where `bin`
// is the package's sole executable.
func binScript(manifest *npmPackage, pkgDir, name string) (string, error) {
	var single string
	if json.Unmarshal(manifest.Bin, &single) == nil && single != "" {
		return filepath.Join(pkgDir, filepath.FromSlash(single)), nil
	}
	var many map[string]string
	if err := json.Unmarshal(manifest.Bin, &many); err != nil || many[name] == "" {
		return "", fmt.Errorf("%s/package.json: bin names no %s", pkgDir, name)
	}
	return filepath.Join(pkgDir, filepath.FromSlash(many[name])), nil
}

// bundleEntry is the entry bun's bundler follows: the bin script
// itself where its extension names a script, else a copy beside it
// under `.mjs` for a package of type module and `.cjs` otherwise — an
// extensionless file, the usual shape of an npm bin, bun bundles as
// an opaque file, the executable then running nothing.
func bundleEntry(script, packageType string) (string, error) {
	switch filepath.Ext(script) {
	case ".js", ".cjs", ".mjs":
		return script, nil
	}
	ext := ".cjs"
	if packageType == "module" {
		ext = ".mjs"
	}
	if err := copyFile(script, script+ext); err != nil {
		return "", err
	}
	return script + ext, nil
}
