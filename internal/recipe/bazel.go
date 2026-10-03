package recipe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildBazel fetches the source archive, extracts it with the
// recipe's leading components stripped, copies the recipe's own files
// (plugins/<name>/files) into the source tree at the directory the
// recipe names, builds the target with bazel on the host
// itself and copies the output to the tree. bazelisk is preferred
// where present so the source's own .bazelversion picks the bazel.
func buildBazel(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	if len(platforms) != 1 {
		return fmt.Errorf("%s: a bazel recipe builds one platform, the host's", name)
	}
	pl := platforms[0]
	// bazel's own paths nest deep; on windows the source tree sits
	// under a short root so the build's paths stay within the
	// platform's limit.
	root := ""
	if runtime.GOOS == "windows" {
		root = `C:\b`
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
	}
	src, err := os.MkdirTemp(root, "src-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	url := catalog.Expand(p.Archive, version, pl)
	if err := extractTarInto(ctx, url, p.Strip, src); err != nil {
		return err
	}
	if p.Files != "" {
		from := filepath.Join(c.Dir, "plugins", filepath.FromSlash(name), "files")
		if err := copyTree(from, filepath.Join(src, p.Files), irregularRefused); err != nil {
			return err
		}
	}
	bazel := "bazelisk"
	if _, err := exec.LookPath(bazel); err != nil {
		bazel = "bazel"
	}
	args := bazelArgs(p, runtime.GOOS)
	// A source tree's own `tools/bazel` wrapper is a shell script,
	// which bazelisk cannot run on windows; bazelisk runs the bazel
	// the tree's .bazelversion names directly instead.
	env := []string{"BAZELISK_SKIP_WRAPPER=true"}
	if err := run(ctx, src, env, bazel, args...); err != nil {
		return err
	}
	built := filepath.Join(src, filepath.FromSlash(catalog.Expand(p.Output, version, pl)))
	if err := layDown(p.Kind, name, pl, built, filepath.Join(TreeDir(out, pl), p.Entrypoint)); err != nil {
		return err
	}
	// The build's server holds the output base open; it is stopped
	// so the source tree can be removed.
	_ = run(ctx, src, env, bazel, "shutdown")
	return nil
}

// bazelArgs composes the build: the recipe's startup options for the
// operating system, the output root kept short on windows, then
// `build -c opt` with the recipe's build options and the target.
func bazelArgs(p *catalog.Plugin, goos string) []string {
	opts := p.Options[goos]
	args := append([]string{}, opts.Startup...)
	if goos == "windows" {
		args = append(args, "--output_user_root=C:/b/out")
	}
	args = append(args, "--host_jvm_args=-Djava.net.preferIPv4Stack=true", "build", "-c", "opt")
	args = append(args, opts.Build...)
	return append(args, p.Target)
}

// extractTarInto fetches a gzip-compressed tar and extracts it into
// dir with its leading path components stripped (untar).
func extractTarInto(ctx context.Context, url string, strip int, dir string) error {
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	body, err := web.Open(ctx, url, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	return untar(body, strip, dir)
}
