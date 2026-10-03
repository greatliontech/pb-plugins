package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// buildDart fetches the repository's archive at the version's tag,
// resolves the package's dependencies with pub in its directory (a
// pub workspace resolving from the repository's root; a package
// committing no lockfile resolves as its manifest allows, as
// upstream's own builds do) and compiles the recipe's script to an
// executable on the platform itself — the host's — with the Dart
// SDK the pipeline pinned, then lays it down as the tree's
// entrypoint, held to load elsewhere than this host (layDown; the
// runtime links the C library, so the linux tree takes the base).
func buildDart(ctx context.Context, _ *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	if len(platforms) != 1 {
		return fmt.Errorf("%s: a dart recipe builds one platform, the host's", name)
	}
	pl := platforms[0]
	src, err := fetchTag(ctx, p, version, pl, "pb-plugins-dart-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	dir := filepath.Join(src, filepath.FromSlash(p.Dir))
	if err := run(ctx, dir, nil, "dart", "pub", "get"); err != nil {
		return err
	}
	built := filepath.Join(src, catalog.Expand(p.Entrypoint+"{exe}", version, pl))
	if err := run(ctx, dir, nil, "dart", dartCompileArgs(filepath.FromSlash(p.Main), built)...); err != nil {
		return err
	}
	return layDown(p.Kind, name, pl, built, filepath.Join(TreeDir(out, pl), p.Entrypoint))
}

// dartCompileArgs are dart's arguments: the script compiled to a
// standalone executable at out.
func dartCompileArgs(main, out string) []string {
	return []string{"compile", "exe", main, "-o", out}
}
