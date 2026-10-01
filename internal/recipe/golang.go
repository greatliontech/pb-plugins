package recipe

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// buildGo cross-compiles the main package from a throwaway module
// requiring the plugin's module at the version: CGO disabled, paths
// trimmed, symbols and the build id stripped, so the executable is a
// function of the toolchain and the module alone.
func buildGo(ctx context.Context, p *catalog.Plugin, version string, platforms []string, out string) error {
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
	if err := run(ctx, tmp, env, "go", "get", p.Module+"@"+version); err != nil {
		return err
	}
	pkg := p.Module
	if p.Package != "." {
		pkg = p.Module + "/" + strings.TrimPrefix(p.Package, "./")
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
