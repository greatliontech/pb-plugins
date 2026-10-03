package recipe

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildJvm lays a jvm recipe's trees down for every platform from
// this host: per platform a runtime linked by jlink from the pinned
// JDK's modules — the JDK's archive for that platform fetched from
// Adoptium at the checksum it publishes, its jmods the link's input,
// the host's jlink of the same release doing the linking — under
// `jre/`, its Mach-O files — the launcher and the libraries it
// loads — held to layDown's darwin rule where they lie (the launcher
// linked under its bare name on every platform, as every entrypoint
// is),
// and the jar fetched once from Maven Central at its coordinates,
// verified against the digest Maven publishes beside it, laid down
// as `<entrypoint>.jar`; the image's argv is the catalog's
// (Plugin.Argv). The runtime links the C library, so the linux trees
// take the base.
func buildJvm(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	release := c.Toolchains[string(catalog.KindJvm)]
	jar, err := os.CreateTemp("", "pb-plugins-jar-")
	if err != nil {
		return err
	}
	defer os.Remove(jar.Name())
	defer jar.Close()
	if err := fetchJar(ctx, p, version, jar); err != nil {
		return err
	}
	for _, pl := range platforms {
		jmods, err := fetchJmods(ctx, release, pl)
		if err != nil {
			return err
		}
		tree := TreeDir(out, pl)
		if err := jlink(ctx, jmods, p.JvmModules(), filepath.Join(tree, "jre")); err != nil {
			return err
		}
		launcher := filepath.Join(tree, "jre", "bin", "java")
		if strings.HasPrefix(pl, "windows/") {
			// The windows launcher under its bare name, as every
			// entrypoint is laid down and the image's one argv names
			// it; the launcher finds its libraries by its directory,
			// not its name.
			if err := os.Rename(launcher+".exe", launcher); err != nil {
				return err
			}
		}
		if err := holdRuntime(name, pl, tree, "jre/bin"); err != nil {
			return err
		}
		if _, err := jar.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(tree, p.Entrypoint+".jar"), jar); err != nil {
			return err
		}
	}
	return nil
}

// fetchJar fetches the recipe's jar from Maven Central into f and
// verifies it against the digest Maven publishes beside it (sha256
// where Maven has one, sha1 for every artifact).
func fetchJar(ctx context.Context, p *catalog.Plugin, version string, f *os.File) error {
	group, artifact, _ := strings.Cut(p.Maven, ":")
	url := endpoints.Maven + "/" + strings.ReplaceAll(group, ".", "/") + "/" + artifact + "/" + strings.TrimPrefix(version, "v") + "/" + p.JarFile(version)
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	tmp, _, err := download(ctx, url)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := verify(ctx, tmp, url, p.Checksum); err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(f, tmp)
	return err
}

// staleExtraction is how old an extraction left beside the cache
// must be to be swept: well past any extraction's duration, so one
// in progress beside a sweeping build is never touched.
const staleExtraction = 2 * time.Hour

// sha256RE is the shape of a sha256 as Adoptium publishes one; a
// checksum names a cache directory, so nothing else passes as one.
var sha256RE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// JDKAsset is the Temurin JDK of the release for the platform as
// Adoptium lists it: the archive's URL and the sha256 Adoptium
// publishes with it, held to a sha256's shape.
func JDKAsset(ctx context.Context, release, platform string) (link, checksum string, err error) {
	goos, goarch := catalog.SplitPlatform(platform)
	os_ := map[string]string{"linux": "linux", "darwin": "mac", "windows": "windows"}[goos]
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[goarch]
	api := fmt.Sprintf("%s/v3/assets/release_name/eclipse/jdk-%s?os=%s&architecture=%s&image_type=jdk&heap_size=normal", endpoints.Adoptium, strings.ReplaceAll(release, "+", "%2B"), os_, arch)
	body, err := web.Get(ctx, api, map[string]string{"User-Agent": endpoints.UserAgent})
	if err != nil {
		return "", "", err
	}
	var doc struct {
		Binaries []struct {
			Package struct {
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binaries"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", err
	}
	if len(doc.Binaries) != 1 || doc.Binaries[0].Package.Link == "" || !sha256RE.MatchString(doc.Binaries[0].Package.Checksum) {
		return "", "", fmt.Errorf("jdk %s for %s: Adoptium lists no one asset with a sha256", release, platform)
	}
	return doc.Binaries[0].Package.Link, strings.ToLower(doc.Binaries[0].Package.Checksum), nil
}

// jdkCache is where a release's JDKs are kept extracted per platform
// once fetched, under the user's cache directory (the temporary
// directory where the host names none): a build links every
// platform's runtime and a live test every plugin's, and a JDK is
// fetched once for all of them; a test points it elsewhere. A JDK
// lies under the checksum Adoptium publishes for it, extracted
// beside its place and moved in whole, so a directory in place is a
// complete extraction of those bytes and is never removed; the
// user's own directory, which another user of the host cannot plant.
var jdkCache = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "pb-plugins", "jdk")
}()

// fetchJmods returns the jmods directory of the Temurin JDK of the
// release for the platform, fetched from Adoptium at the checksum it
// publishes and kept under the cache, where a JDK in place under
// that checksum is not fetched again.
func fetchJmods(ctx context.Context, release, platform string) (string, error) {
	link, checksum, err := JDKAsset(ctx, release, platform)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(jdkCache, release, strings.ReplaceAll(platform, "/", "-"), checksum)
	if jmods, err := findDir(dir, "jmods"); err == nil {
		return jmods, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	// Extracted beside its place and moved in whole; two builds
	// missing the cache together both extract, the second's move
	// finding the first's in place and keeping it. An extraction a
	// killed build left beside is swept on a later miss once it is
	// older than any extraction runs, so one in progress beside is
	// left alone.
	if stale, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".extract-*")); len(stale) > 0 {
		for _, s := range stale {
			if st, err := os.Stat(s); err == nil && time.Since(st.ModTime()) > staleExtraction {
				os.RemoveAll(s)
			}
		}
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".extract-")
	if err != nil {
		return "", err
	}
	if err := fetchJDK(ctx, link, checksum, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		if _, statErr := os.Stat(dir); statErr != nil {
			return "", err
		}
	}
	return findDir(dir, "jmods")
}

// fetchJDK fetches the archive at link, holds it to the checksum and
// extracts it into dir.
func fetchJDK(ctx context.Context, link, checksum, dir string) error {
	tmp, _, err := downloadHeld(ctx, link, checksum, "Adoptium")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if strings.HasSuffix(link, ".zip") {
		err = unzipInto(tmp, dir)
	} else {
		err = untarInto(tmp, dir)
	}
	if err != nil {
		return err
	}
	_, err = findDir(dir, "jmods")
	return err
}

// jlink links a runtime from the jmods with the modules into out:
// the class files' debug attributes stripped, no header files or
// man pages, the resources compressed; the native launcher is left
// as the JDK built it.
func jlink(ctx context.Context, jmods string, modules []string, out string) error {
	return run(ctx, "", nil, "jlink", jlinkArgs(jmods, modules, out)...)
}

// jlinkArgs are jlink's arguments for the link.
func jlinkArgs(jmods string, modules []string, out string) []string {
	return []string{"--module-path", jmods, "--add-modules", strings.Join(modules, ","), "--strip-java-debug-attributes", "--no-header-files", "--no-man-pages", "--compress", "zip-6", "--output", out}
}

// findDir finds the one directory of the name under root, at any
// depth (a JDK archive's `jmods` lies under its top directory, on
// darwin under `Contents/Home`).
func findDir(root, name string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == name {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("%s: no %s directory in the archive", root, name)
	}
	return found, nil
}

// untarInto extracts a gzip-compressed tar whole into dir (untar
// with nothing stripped).
func untarInto(f io.Reader, dir string) error { return untar(f, 0, dir) }

// untar extracts a gzip-compressed tar into dir, each entry's
// leading path components stripped by the count (an entry with no
// more passed over): regular files and directories, the modes kept,
// a symbolic link created where its target stays in the archive
// and refused where it escapes, a hard link passed over, a path
// escaping the directory refused.
func untar(f io.Reader, strip int, dir string) error {
	gz, err := gzip.NewReader(f)
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
		parts := strings.Split(path.Clean(h.Name), "/")
		if len(parts) <= strip {
			continue
		}
		rel := path.Join(parts[strip:]...)
		if rel == "." {
			continue
		}
		dst, err := within(dir, rel)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) || strings.HasPrefix(path.Clean(path.Join(path.Dir(rel), h.Linkname)), "../") {
				return fmt.Errorf("%s: link %s escapes the archive", h.Name, h.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(filepath.FromSlash(h.Linkname), dst); err != nil {
				return err
			}
		}
	}
}

// unzipInto extracts a zip whole into dir, the modes kept where the
// archive carries them, a path escaping the directory refused.
func unzipInto(f *os.File, dir string) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return err
	}
	for _, e := range zr.File {
		dst, err := within(dir, e.Name)
		if err != nil {
			return err
		}
		if e.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		r, err := e.Open()
		if err != nil {
			return err
		}
		w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, e.Mode()&0o777|0o600)
		if err != nil {
			r.Close()
			return err
		}
		_, err = io.Copy(w, r)
		r.Close()
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// within is the path of an archive entry under dir, refused where
// it would escape.
func within(dir, name string) (string, error) {
	dst := filepath.Join(dir, filepath.FromSlash(name))
	if dst != dir && !strings.HasPrefix(dst, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: an archive entry outside the directory", name)
	}
	return dst, nil
}
