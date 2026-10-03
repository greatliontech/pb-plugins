package recipe

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildPython lays a python recipe's trees down for every platform
// from this host: per platform the package installed from PyPI at
// the version with its dependencies by uv, which chooses that
// platform's wheels and reads the dependencies' markers for it
// rather than for the host, under `app/`, a launcher `app/<entrypoint>.py`
// written from the console script the package's metadata names (held
// to name the recipe's entrypoint), and the standalone CPython build
// for the platform (python-build-standalone's, at the release the
// catalog pins and the checksum the release publishes in SHA256SUMS)
// under `python/`, its interpreter laid down as `python/bin/python3`
// on every platform, no link left in the tree; uv and the host's own
// interpreter, which uv installs under, fetched at their published
// checksums and kept under the cache. The image's argv is the
// catalog's (Plugin.Argv). CPython links the C library, so the linux
// trees take the base.
func buildPython(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	release, uvVersion := c.Toolchains["python"], c.Toolchains["uv"]
	pyVersion, tag, _ := strings.Cut(release, "+")
	sums, err := pythonChecksums(ctx, tag)
	if err != nil {
		return err
	}
	uv, err := fetchUv(ctx, uvVersion)
	if err != nil {
		return err
	}
	hostPython, err := fetchHostPython(ctx, release, sums)
	if err != nil {
		return err
	}
	for _, pl := range platforms {
		tree := TreeDir(out, pl)
		app := filepath.Join(tree, "app")
		// uv under no configuration of the host's: its files passed
		// over, its variables dropped from the environment, the
		// index named, so the image holds PyPI's packages whatever
		// the runner holds.
		if err := runIn(ctx, tree, environWithout("UV_"), uv, "pip", "install", "--no-config", "--default-index", endpoints.PyPISimple, "--python", hostPython, "--python-platform", pythonTriple(pl), "--python-version", pyVersion, "--only-binary", ":all:", "--no-cache", "--target", app, p.Pypi+"=="+strings.TrimPrefix(version, "v")); err != nil {
			return err
		}
		// The scripts uv writes under app/bin run nothing here: their
		// shebangs name the host's interpreter.
		if err := os.RemoveAll(filepath.Join(app, "bin")); err != nil {
			return err
		}
		entry, err := consoleScript(app, p.Pypi, p.Entrypoint)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(app, p.Entrypoint+".py"), []byte(launcher(p.Entrypoint, entry)), 0o644); err != nil {
			return err
		}
		if err := fetchPython(ctx, release, pl, sums, tree, runtimeFiles); err != nil {
			return err
		}
		if err := layInterpreter(tree, pl, pyVersion); err != nil {
			return err
		}
		if err := holdRuntime(name, pl, tree, "python/bin"); err != nil {
			return err
		}
	}
	return nil
}

// pythonTriple is the platform as python-build-standalone and uv
// name it, a target triple.
func pythonTriple(platform string) string {
	goos, goarch := catalog.SplitPlatform(platform)
	arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[goarch]
	os_ := map[string]string{"linux": "unknown-linux-gnu", "darwin": "apple-darwin", "windows": "pc-windows-msvc"}[goos]
	return arch + "-" + os_
}

// pythonArchive is the release's archive for the platform as
// python-build-standalone names it: the stripped install-only build.
func pythonArchive(release, platform string) string {
	return "cpython-" + release + "-" + pythonTriple(platform) + "-install_only_stripped.tar.gz"
}

// pythonChecksums reads the release's SHA256SUMS: the sha256 of each
// archive by its name.
func pythonChecksums(ctx context.Context, tag string) (map[string]string, error) {
	return publishedSums(ctx, endpoints.PythonStandalone+"/"+tag+"/SHA256SUMS")
}

// runtimeFiles admits the build's files a runtime needs: the
// interpreter, its libraries and the standard library, under
// `python/`; not its headers, manuals, terminal descriptions and
// the like under `share/` and `include/`, whose names collide by
// case on a host that folds it.
func runtimeFiles(name string) bool {
	for _, dir := range []string{"python/share", "python/include"} {
		if name == dir || strings.HasPrefix(name, dir+"/") {
			return false
		}
	}
	return true
}

// fetchPython fetches the release's archive for the platform, holds
// it to the checksum the release publishes, and extracts the files
// keep admits into the tree, under `python/` as the archive lays
// them, no link among them.
func fetchPython(ctx context.Context, release, platform string, sums map[string]string, tree string, keep func(string) bool) error {
	_, tag, _ := strings.Cut(release, "+")
	archive := pythonArchive(release, platform)
	want, ok := sums[archive]
	if !ok {
		return fmt.Errorf("python-build-standalone %s: SHA256SUMS names no %s", tag, archive)
	}
	tmp, _, err := downloadHeld(ctx, endpoints.PythonStandalone+"/"+tag+"/"+archive, want, "python-build-standalone")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	return untarSelect(tmp, tree, keep)
}

// fetchHostPython is the host's own interpreter of the release, which
// uv installs under, kept under the cache as the build lays it
// (`bin/python3.<minor>`, no link).
func fetchHostPython(ctx context.Context, release string, sums map[string]string) (string, error) {
	archive := pythonArchive(release, Host())
	want, ok := sums[archive]
	if !ok {
		return "", fmt.Errorf("python-build-standalone: SHA256SUMS names no %s, the host's", archive)
	}
	dir, err := cached("python", release, Host(), want, func(dir string) error {
		return fetchPython(ctx, release, Host(), sums, dir, runtimeFiles)
	})
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "python", "python.exe"), nil
	}
	pyVersion, _, _ := strings.Cut(release, "+")
	parts := strings.SplitN(pyVersion, ".", 3)
	return filepath.Join(dir, "python", "bin", "python"+strings.Join(parts[:2], ".")), nil
}

// fetchUv is uv for the host at the version, fetched from its
// release at the sha256 published beside it and kept under the cache.
func fetchUv(ctx context.Context, version string) (string, error) {
	triple := pythonTriple(Host())
	archive, member := "uv-"+triple+".tar.gz", "uv-"+triple+"/uv"
	if runtime.GOOS == "windows" {
		archive, member = "uv-"+triple+".zip", "uv.exe"
	}
	url := endpoints.Uv + "/" + version + "/" + archive
	published, err := web.Get(ctx, url+".sha256", nil)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(published))
	if len(fields) == 0 || !sha256RE.MatchString(fields[0]) {
		return "", fmt.Errorf("%s.sha256: no sha256 first on its line", url)
	}
	dir, err := cached("uv", version, Host(), strings.ToLower(fields[0]), func(dir string) error {
		return fetchMember(ctx, url, fields[0], "uv's release", member, filepath.Join(dir, filepath.Base(member)))
	})
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, "uv.exe"), nil
	}
	return filepath.Join(dir, "uv"), nil
}

// consoleScript reads the console script named `name` among the
// package's own entry points under app (its distribution the one
// whose directory names the package, the names compared normalized
// since a wheel may spell its own as the project does; a
// dependency's script of the same name not taken for it): the
// object it runs, `module:attr`, an extras suffix dropped.
func consoleScript(app, pkg, name string) (string, error) {
	entries, err := os.ReadDir(app)
	if err != nil {
		return "", err
	}
	var files []string
	for _, e := range entries {
		dist, ok := strings.CutSuffix(e.Name(), ".dist-info")
		if !ok {
			continue
		}
		if i := strings.LastIndex(dist, "-"); i > 0 && normalizedName(dist[:i]) == normalizedName(pkg) {
			files = append(files, filepath.Join(app, e.Name(), "entry_points.txt"))
		}
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		section := ""
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if section != "console_scripts" || !ok || strings.TrimSpace(key) != name {
				continue
			}
			object, _, _ := strings.Cut(strings.TrimSpace(value), "[")
			module, attr, ok := strings.Cut(strings.TrimSpace(object), ":")
			if !ok || module == "" || attr == "" {
				return "", fmt.Errorf("console script %s runs %q, no module:attr", name, strings.TrimSpace(value))
			}
			return module + ":" + attr, nil
		}
	}
	return "", fmt.Errorf("the package's console scripts name no %s, the recipe's entrypoint", name)
}

// normalizedName is a package's name as a wheel's metadata directory
// spells it: lowercase, every run of dots, hyphens and underscores
// one underscore.
func normalizedName(pkg string) string {
	var b strings.Builder
	run := false
	for _, r := range strings.ToLower(pkg) {
		if r == '-' || r == '_' || r == '.' {
			run = true
			continue
		}
		if run {
			b.WriteByte('_')
			run = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// launcher is the script the image's process runs: the console
// script's object, imported with the package's files beside the
// launcher on the path (the interpreter runs isolated, adding
// nothing itself).
func launcher(name, entry string) string {
	module, attr, _ := strings.Cut(entry, ":")
	return fmt.Sprintf(`# %s: the console script the package names, run by the interpreter beside it.
import importlib
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
entry = importlib.import_module(%q)
for attr in %q.split("."):
    entry = getattr(entry, attr)
sys.exit(entry())
`, name, module, attr)
}

// layInterpreter lays the extracted build's interpreter down as
// `python/bin/python3`, the same name on every platform, as the
// image's argv names it: on linux and darwin the build's `bin/`
// holds the interpreter as `python3.<minor>` beside scripts and the
// links to it (not laid down), which are dropped with the rest of
// `bin/`; on windows the build holds
// `python.exe` and its libraries at `python/`, and the interpreter
// moves under `bin/` with the libraries it loads by name beside it
// and a `python3._pth` naming the standard library's directories,
// which CPython reads beside an interpreter so named instead of
// deriving them from its own place. No link is left anywhere under
// `python/`, an image carrying none.
func layInterpreter(tree, platform, pyVersion string) error {
	root := filepath.Join(tree, "python")
	bin := filepath.Join(root, "bin")
	goos, _ := catalog.SplitPlatform(platform)
	if goos == "windows" {
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(root, "python.exe"), filepath.Join(bin, "python3")); err != nil {
			return err
		}
		os.Remove(filepath.Join(root, "pythonw.exe"))
		dlls, _ := filepath.Glob(filepath.Join(root, "*.dll"))
		for _, dll := range dlls {
			if err := os.Rename(dll, filepath.Join(bin, filepath.Base(dll))); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(bin, "python3._pth"), []byte("../Lib\n../DLLs\n"), 0o644); err != nil {
			return err
		}
	} else {
		parts := strings.SplitN(pyVersion, ".", 3)
		target := "python" + strings.Join(parts[:2], ".")
		if fi, err := os.Stat(filepath.Join(bin, target)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s: the build holds no interpreter %s: %v", platform, target, err)
		}
		entries, err := os.ReadDir(bin)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name() != target {
				if err := os.RemoveAll(filepath.Join(bin, e.Name())); err != nil {
					return err
				}
			}
		}
		if err := os.Rename(filepath.Join(bin, target), filepath.Join(bin, "python3")); err != nil {
			return err
		}
	}
	return dropLinks(root)
}
