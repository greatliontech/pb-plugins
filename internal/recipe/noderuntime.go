package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
)

// buildNodeRuntime lays a runtime node recipe's trees down for every
// platform from this host: the npm package installed at the version
// with its dependencies, no lifecycle script run, its files copied
// under `app/` in every tree (the links npm lays down passed over, an
// image carrying no link), the package's `bin` held to name the
// recipe's script; node's own binary for each platform fetched
// from nodejs.org at the pinned version, held to the checksum
// nodejs.org publishes in the release's SHASUMS256.txt, laid down as
// `/node` under its bare name on every platform; the image's argv the
// catalog's (Plugin.Argv). node links the C library, so the linux
// trees take the base.
func buildNodeRuntime(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	tmp, err := os.MkdirTemp("", "pb-plugins-node-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := writeManifest(tmp, p, version); err != nil {
		return err
	}
	if err := run(ctx, tmp, nil, "npm", "install", "--ignore-scripts", "--omit=dev", "--no-audit", "--no-fund", "--loglevel=error"); err != nil {
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
	if want := filepath.Join(pkgDir, filepath.FromSlash(p.Script)); script != want {
		return fmt.Errorf("%s: the package's bin names %s, the recipe's script %s", name, script, want)
	}
	release := c.Toolchains[string(catalog.KindNode)]
	sums, err := nodeChecksums(ctx, release)
	if err != nil {
		return err
	}
	for _, pl := range platforms {
		tree := TreeDir(out, pl)
		modules := filepath.Join(tmp, "node_modules")
		if err := copyTree(modules, filepath.Join(tree, "app", "node_modules"), func(p string) error { return modulesLink(modules, p) }); err != nil {
			return err
		}
		if err := fetchNode(ctx, release, pl, sums, filepath.Join(tree, "node")); err != nil {
			return err
		}
		if err := holdExecutable(p.Kind, name, pl, filepath.Join(tree, "node"), "."); err != nil {
			return err
		}
	}
	return nil
}

// modulesLink decides a link under an installed node_modules at
// modules: the links npm lays down under a `.bin` directory, which
// point at the packages' own scripts, are passed over (an image
// carries no link, and the image's process names the package's
// script itself); any other, a dependency laid down as a link, is
// refused, since the tree would lack it.
func modulesLink(modules, path string) error {
	rel, _ := filepath.Rel(modules, path)
	if strings.Contains("/"+filepath.ToSlash(rel), "/.bin/") {
		return nil
	}
	return fmt.Errorf("%s: a link among the package's files, which the tree carries none of", filepath.ToSlash(rel))
}

// nodeArchive is the release's archive for the platform as nodejs.org
// names it, and the path of node's binary within it.
func nodeArchive(release, platform string) (archive, member string) {
	goos, goarch := catalog.SplitPlatform(platform)
	os_ := map[string]string{"linux": "linux", "darwin": "darwin", "windows": "win"}[goos]
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	base := "node-v" + release + "-" + os_ + "-" + arch
	if goos == "windows" {
		return base + ".zip", base + "/node.exe"
	}
	return base + ".tar.gz", base + "/bin/node"
}

// nodeChecksums reads the release's SHASUMS256.txt: the sha256 of
// each archive by its name.
func nodeChecksums(ctx context.Context, release string) (map[string]string, error) {
	return publishedSums(ctx, endpoints.NodeDist+"/v"+release+"/SHASUMS256.txt")
}

// fetchNode fetches the release's archive for the platform, holds it
// to the checksum nodejs.org publishes, and lays node's binary down
// at out.
func fetchNode(ctx context.Context, release, platform string, sums map[string]string, out string) error {
	archive, member := nodeArchive(release, platform)
	want, ok := sums[archive]
	if !ok {
		return fmt.Errorf("node v%s: SHASUMS256.txt names no %s", release, archive)
	}
	return fetchMember(ctx, endpoints.NodeDist+"/v"+release+"/"+archive, want, "nodejs.org", member, out)
}
