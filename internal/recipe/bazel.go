package recipe

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
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
		if err := copyTree(from, filepath.Join(src, p.Files)); err != nil {
			return err
		}
	}
	bazel := "bazelisk"
	if _, err := exec.LookPath(bazel); err != nil {
		bazel = "bazel"
	}
	args := []string{}
	if runtime.GOOS == "windows" {
		args = append(args, "--output_user_root=C:/b/out")
	}
	args = append(args, "--host_jvm_args=-Djava.net.preferIPv4Stack=true", "build", "-c", "opt", p.Target)
	// A source tree's own `tools/bazel` wrapper is a shell script,
	// which bazelisk cannot run on windows; bazelisk runs the bazel
	// the tree's .bazelversion names directly instead.
	env := []string{"BAZELISK_SKIP_WRAPPER=true"}
	if err := run(ctx, src, env, bazel, args...); err != nil {
		return err
	}
	built := filepath.Join(src, filepath.FromSlash(catalog.Expand(p.Output, version, pl)))
	if err := copyFile(built, filepath.Join(TreeDir(out, pl), p.Entrypoint)); err != nil {
		return err
	}
	// The build's server holds the output base open; it is stopped
	// so the source tree can be removed.
	_ = run(ctx, src, env, bazel, "shutdown")
	return nil
}

// extractTarInto fetches a gzip-compressed tar and extracts its
// regular files, directories and symbolic links under dir, the
// leading strip components of every name dropped; an entry escaping
// dir is refused.
func extractTarInto(ctx context.Context, url string, strip int, dir string) error {
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		parts := strings.Split(name, "/")
		if len(parts) <= strip {
			continue
		}
		rel := path.Join(parts[strip:]...)
		if rel == "." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) || strings.HasPrefix(path.Clean(path.Join(path.Dir(rel), h.Linkname)), "../") {
				return fmt.Errorf("%s: link %s escapes the archive", url, name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(h.Linkname), target); err != nil {
				return err
			}
		}
	}
}

// copyTree copies the regular files under from to the same paths
// under to.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		return copyFile(p, filepath.Join(to, rel))
	})
}
