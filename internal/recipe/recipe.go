// Package recipe builds a plugin's platform trees: for each platform
// asked, a directory holding the entrypoint executable at its root,
// laid out as out/<os>-<arch>/<entrypoint>, the shape `pb plugin
// build` packages. Each kind of the catalog has its builder here; a
// native kind builds only the host's platform, the others every
// platform asked from one host.
package recipe

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// TreeDir is the directory of a platform's tree under out.
func TreeDir(out, platform string) string {
	os, arch := catalog.SplitPlatform(platform)
	return filepath.Join(out, os+"-"+arch)
}

// Host is the host's platform, `os/arch`.
func Host() string { return runtime.GOOS + "/" + runtime.GOARCH }

// Build produces the trees of the plugin named at the version for the
// platforms, each a platform the plugin serves, under out. A native
// kind refuses any platform but the host's, naming both.
func Build(ctx context.Context, c *catalog.Catalog, name, version string, platforms []string, out string) error {
	p, ok := c.Plugins[name]
	if !ok {
		return fmt.Errorf("%s: not in the catalog", name)
	}
	// The builders run their tools in a throwaway directory, so the
	// trees' location is fixed before any of them runs.
	out, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	served := map[string]bool{}
	for _, pl := range p.PlatformsOf() {
		served[pl] = true
	}
	for _, pl := range platforms {
		if !served[pl] {
			return fmt.Errorf("%s: does not serve %s", name, pl)
		}
		if p.Kind.Native() && pl != Host() {
			return fmt.Errorf("%s: a %s recipe builds on the platform itself; the host is %s, not %s", name, p.Kind, Host(), pl)
		}
		if err := os.MkdirAll(TreeDir(out, pl), 0o755); err != nil {
			return err
		}
	}
	switch p.Kind {
	case catalog.KindGo:
		return buildGo(ctx, p, version, platforms, out)
	case catalog.KindNode:
		return buildNode(ctx, p, version, platforms, out)
	case catalog.KindRelease:
		return buildRelease(ctx, p, version, platforms, out)
	case catalog.KindBazel:
		return buildBazel(ctx, c, name, p, version, platforms, out)
	}
	return fmt.Errorf("%s: unknown kind %q", name, p.Kind)
}

// run executes a command in dir with the environment added to the
// process's own, its output streamed to the process's standard error.
func run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "+ %s %s\n", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// copyFile writes src's bytes to dst, creating dst's directory.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFile(dst, in)
}

func writeFile(dst string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
