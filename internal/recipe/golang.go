package recipe

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/github"
)

// buildGo cross-compiles the main package from a throwaway module
// requiring the plugin's module at the version — at the commit the
// version's tag names where the recipe builds from a repository tag
// the module proxy does not list, the module's path read from its
// go.mod at that commit and the proxy serving the module there as a
// pseudo-version — CGO disabled, paths trimmed, symbols and the
// build id stripped, so the executable is a function of the
// toolchain and the module alone.
func buildGo(ctx context.Context, p *catalog.Plugin, version string, platforms []string, out string) error {
	module, at := p.Module, version
	if p.Repository != "" {
		tag := catalog.Expand(p.Tag, version, "")
		commit, err := github.TagCommit(ctx, p.Repository, tag)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", p.Repository, tag, err)
		}
		gomod, err := github.File(ctx, p.Repository, commit, path.Join(p.Dir, "go.mod"))
		if err != nil {
			return fmt.Errorf("%s at %s: %w", p.Repository, tag, err)
		}
		module = modfile.ModulePath(gomod)
		if module == "" {
			return fmt.Errorf("%s at %s: %s names no module", p.Repository, tag, path.Join(p.Dir, "go.mod"))
		}
		at = commit
	}
	tmp, err := os.MkdirTemp("", "pb-plugins-go-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module tree\n\ngo 1.24\n"), 0o644); err != nil {
		return err
	}
	// The toolchain the module's go directive asks for is fetched
	// where the host's is older, so a plugin never fails to build for
	// the host's toolchain lagging its module.
	env := []string{"GOTOOLCHAIN=auto", "GOFLAGS=-mod=mod"}
	if err := run(ctx, tmp, env, "go", "get", module+"@"+at); err != nil {
		return err
	}
	pkg := module
	if p.Package != "." {
		pkg = module + "/" + strings.TrimPrefix(p.Package, "./")
	}
	for _, pl := range platforms {
		goos, goarch := catalog.SplitPlatform(pl)
		outfile := filepath.Join(TreeDir(out, pl), p.Entrypoint)
		penv := append(env, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if err := run(ctx, tmp, penv, "go", goBuildArgs(p.Tags, outfile, pkg)...); err != nil {
			return err
		}
	}
	return nil
}

// goBuildArgs are the build's arguments: paths trimmed, symbols and
// the build id stripped, the recipe's tags where it has any, the
// package last.
func goBuildArgs(tags []string, outfile, pkg string) []string {
	args := []string{"build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", outfile}
	if len(tags) > 0 {
		args = append(args, "-tags", strings.Join(tags, ","))
	}
	return append(args, pkg)
}
