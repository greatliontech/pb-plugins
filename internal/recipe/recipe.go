// Package recipe builds a plugin's platform trees: for each platform
// asked, a directory holding the plugin's process as the image's
// argv names it — the entrypoint executable at its root for most
// kinds, laid out as out/<os>-<arch>/<entrypoint>; a runtime beside
// the program for the jvm kind — the shape `pb plugin build`
// packages. Each kind of the catalog has its builder here; a native
// kind builds only the host's platform, the others every platform
// asked from one host.
package recipe

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
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
	build, ok := builders[p.Kind]
	if !ok {
		return fmt.Errorf("%s: unknown kind %q", name, p.Kind)
	}
	if err := build(ctx, c, name, p, version, platforms, out); err != nil {
		return err
	}
	// The host's tree, where one was built, answers the probe before
	// any tree is handed on: what a kind produces is held to the
	// plugin protocol, not to its toolchain's exit code.
	return probeTree(ctx, p, platforms, out)
}

// A builder produces the plugin's trees for the platforms under out.
type builder func(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error

// builders are the kinds' builders; a test swaps one in.
var builders = map[catalog.Kind]builder{
	catalog.KindGo: func(ctx context.Context, _ *catalog.Catalog, _ string, p *catalog.Plugin, version string, platforms []string, out string) error {
		return buildGo(ctx, p, version, platforms, out)
	},
	catalog.KindNode: func(ctx context.Context, c *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
		if p.Runtime {
			return buildNodeRuntime(ctx, c, name, p, version, platforms, out)
		}
		return buildNode(ctx, p, version, platforms, out)
	},
	catalog.KindRelease: func(ctx context.Context, _ *catalog.Catalog, _ string, p *catalog.Plugin, version string, platforms []string, out string) error {
		return buildRelease(ctx, p, version, platforms, out)
	},
	catalog.KindBazel:  buildBazel,
	catalog.KindRust:   buildRust,
	catalog.KindSwift:  buildSwift,
	catalog.KindDart:   buildDart,
	catalog.KindJvm:    buildJvm,
	catalog.KindPython: buildPython,
}

// command is a command in dir with the environment added to the
// process's own, its standard error the process's.
func command(ctx context.Context, dir string, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stderr = os.Stderr
	fmt.Fprintf(os.Stderr, "+ %s %s\n", name, strings.Join(args, " "))
	return cmd
}

// run executes a command, its output streamed to the process's
// standard error.
func run(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := command(ctx, dir, env, name, args...)
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// runIn is run under the environment given whole, not the process's.
func runIn(ctx context.Context, dir string, environ []string, name string, args ...string) error {
	cmd := command(ctx, dir, nil, name, args...)
	cmd.Env = environ
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// environWithout is the process's environment less the variables
// whose names bear the prefix in any case: windows reads a variable's
// name without regard to case, so `uv_index_url` is `UV_INDEX_URL`
// there.
func environWithout(prefix string) []string {
	var environ []string
	for _, kv := range os.Environ() {
		if len(kv) < len(prefix) || !strings.EqualFold(kv[:len(prefix)], prefix) {
			environ = append(environ, kv)
		}
	}
	return environ
}

// output executes a command and returns its standard output.
func output(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := command(ctx, dir, env, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// fetchTag fetches the repository's archive at the version's tag
// into a fresh directory, its one top-level directory stripped, and
// returns the directory, the caller's to remove.
func fetchTag(ctx context.Context, p *catalog.Plugin, version, platform, prefix string) (string, error) {
	src, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/%s/archive/refs/tags/%s.tar.gz", endpoints.GitHub, p.Repository, catalog.Expand(p.Tag, version, platform))
	if err := extractTarInto(ctx, url, 1, src); err != nil {
		os.RemoveAll(src)
		return "", err
	}
	return src, nil
}

// layDown lays an executable a native kind built down as the
// platform's entrypoint once it is held to load elsewhere than the
// runner that built it: on linux static, for a kind that takes no
// base (the OS sandbox row loads a static entrypoint alone, and a
// baseless image carries no loader; a kind over the base links the
// C library and is not held), on darwin bound to the system's
// libraries alone (a toolchain's library reached through a search
// path is absent on every other machine).
func layDown(kind catalog.Kind, name, platform, built, dst string) error {
	if err := holdExecutable(kind, name, platform, built, "."); err != nil {
		return err
	}
	return copyFile(built, dst)
}

// copyTree copies the regular files under from to the same paths
// under to; irregular decides a file of another kind (a link),
// nil passing it over.
func copyTree(from, to string, irregular func(path string) error) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return irregular(p)
		}
		return copyFile(p, filepath.Join(to, rel))
	})
}

// irregularRefused is copyTree's refusal of every file that is no
// regular file.
func irregularRefused(path string) error {
	return fmt.Errorf("%s: not a regular file", path)
}

// dropLinks removes every symbolic link under dir: an image pb runs
// carries none, and a runtime's build lays some down (jlink's
// `legal/` notices, a CPython build's `bin/` and `lib/` aliases),
// none of which the runtime needs.
func dropLinks(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return os.Remove(p)
		}
		return nil
	})
}

// toolCache is where the tools and runtimes a build fetches are kept
// extracted once fetched, under the user's cache directory (the
// temporary directory where the host names none): a build fetches
// every platform's runtime and a live test every plugin's, and each
// is fetched once for all of them; a test points it elsewhere. A
// tool lies under the checksum its publisher states for it,
// extracted beside its place and moved in whole, so a directory in
// place is a complete extraction of those bytes and is never
// removed; the user's own directory, which another user of the host
// cannot plant.
var toolCache = func() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "pb-plugins")
}()

// cached is the directory under the cache holding the tool's release
// for the platform at the checksum, extracted by `extract` into it
// on a miss. Extracted beside its place and moved in whole; two
// builds missing the cache together both extract, the second's move
// finding the first's in place and keeping it. An extraction a
// killed build left beside is swept on a later miss once it is
// older than any extraction runs, so one in progress beside is left
// alone.
func cached(tool, release, platform, checksum string, extract func(dir string) error) (string, error) {
	dir := filepath.Join(toolCache, tool, release, strings.ReplaceAll(platform, "/", "-"), checksum)
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
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
	if err := extract(tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		if _, statErr := os.Stat(dir); statErr != nil {
			return "", err
		}
	}
	return dir, nil
}

// holdExecutable holds an executable to layDown's rule for the
// platform where it lies; at is the directory within the tree the
// executable occupies, slash-separated, the root `.`.
func holdExecutable(kind catalog.Kind, name, platform, built, at string) error {
	os, _ := catalog.SplitPlatform(platform)
	var err error
	switch os {
	case "linux":
		if !kind.NeedsBase() {
			err = checkStatic(built)
		}
	case "darwin":
		err = checkPortable(built, at, at)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, platform, err)
	}
	return nil
}

// holdRuntime holds a runtime's tree to layDown's darwin rule in
// whole: every Mach-O file under it, the launcher and the libraries
// it loads, a library's `@executable_path` the launcher's directory
// launcherAt. dyld resolves a library's `@rpath` binding through
// the run paths of every image on the chain that loaded it, up to
// the launcher, so the tree is read first and each file held
// knowing whether any file of it binds through `@rpath`: where one
// does, a run path elsewhere is live in every file. A runtime links
// the C library on linux and is not held there. A file is a Mach-O
// by its magic, so one the reader cannot parse is refused rather
// than passed over.
func holdRuntime(name, platform, tree, launcherAt string) error {
	if os, _ := catalog.SplitPlatform(platform); os != "darwin" {
		return nil
	}
	var files []string
	read := map[string][]machoImage{}
	chainBinds := false
	err := filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if is, err := machO(p); err != nil || !is {
			return err
		}
		rel, err := filepath.Rel(tree, p)
		if err != nil {
			return err
		}
		images, err := readMachO(p)
		if err != nil {
			return fmt.Errorf("%s %s: %s: %w", name, platform, filepath.ToSlash(rel), err)
		}
		for _, img := range images {
			chainBinds = chainBinds || img.bindsThroughRpath()
		}
		files = append(files, filepath.ToSlash(rel))
		read[filepath.ToSlash(rel)] = images
		return nil
	})
	if err != nil {
		return err
	}
	for _, rel := range files {
		if err := holdPortable(read[rel], path.Dir(rel), launcherAt, chainBinds); err != nil {
			return fmt.Errorf("%s %s: %s: %w", name, platform, rel, err)
		}
	}
	return nil
}

// machoImage is one architecture of a Mach-O file as the portability
// rule reads it: the libraries its load commands bind and the run
// paths it records, and whether it is an executable.
type machoImage struct {
	libs, rpaths []string
	exec         bool
}

// bindsThroughRpath reports whether a load command names a library
// through `@rpath`.
func (img machoImage) bindsThroughRpath() bool {
	for _, lib := range img.libs {
		if strings.HasPrefix(lib, "@rpath/") {
			return true
		}
	}
	return false
}

// readMachO reads a Mach-O file's architectures, every one of a
// universal file, as the portability rule needs them.
func readMachO(path string) ([]machoImage, error) {
	files, close, err := openMachO(path)
	if err != nil {
		return nil, err
	}
	defer close()
	var images []machoImage
	for _, f := range files {
		libs, rpaths, err := loadedLibraries(f)
		if err != nil {
			return nil, err
		}
		images = append(images, machoImage{libs: libs, rpaths: rpaths, exec: f.Type == macho.TypeExec})
	}
	return images, nil
}

// checkStatic refuses an ELF executable that names an interpreter.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("no ELF executable: %w", err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("dynamically linked: a linux tree of this kind is static")
		}
	}
	return nil
}

// checkPortable refuses a Mach-O executable, every architecture of a
// universal one, that loads a library from anywhere but the system
// (`/usr/lib`, `/System/Library`, the darwin row's substrate) or the
// image itself — a binding or a run
// path through `@executable_path` or `@loader_path`, which a bundled
// runtime's launcher uses for its own libraries — as the darwin
// sandbox row admits: a library named through `@rpath` with no
// image-relative run path, or an absolute path into a toolchain,
// loads on the machine that built it alone. A run path binds
// nothing by itself: one elsewhere than the image or the system (a
// toolchain's, which the swift linker records, or one climbing past
// the tree's root, which dart's runtime carries) is inert where no
// load command of the file names a library through `@rpath`, and
// refused where one does, since dyld would search it (a runtime's
// tree is held as the loading chain it is, by holdRuntime). Every load
// command naming a library counts — the weak ones too, which is how
// a toolchain's compatibility library is bound. A back-deployed
// runtime library bound through `@rpath` with `/usr/lib/swift` among
// absolute run paths alone would load on a recent macOS, and is
// refused here all the same; the remedy is a deployment target the
// system's runtime serves, not a looser rule. at is the file's own
// directory within the tree, launcherAt the directory of the
// executable whose process loads it, where the file is a library.
func checkPortable(path, at, launcherAt string) error {
	images, err := readMachO(path)
	if err != nil {
		return err
	}
	return holdPortable(images, at, launcherAt, false)
}

// holdPortable holds a file's images to checkPortable's rule;
// chainBinds says that some file of the tree, this one or another,
// binds a library through `@rpath`, which makes a run path
// elsewhere live here.
func holdPortable(images []machoImage, at, launcherAt string, chainBinds bool) error {
	for _, img := range images {
		execAt := launcherAt
		if img.exec {
			execAt = at
		}
		relative, elsewhere := false, ""
		for _, rp := range img.rpaths {
			switch {
			case imageRelative(rp, at, execAt):
				relative = true
			case !substrate(rp) && elsewhere == "":
				elsewhere = rp
			}
		}
		if chainBinds && elsewhere != "" && !img.bindsThroughRpath() {
			return fmt.Errorf("searches %s, which the tree's libraries bind through: a darwin tree's run paths are the system's or relative to the image", elsewhere)
		}
		for _, lib := range img.libs {
			switch {
			case substrate(lib), imageRelative(lib, at, execAt):
			case strings.HasPrefix(lib, "@rpath/") && elsewhere != "":
				return fmt.Errorf("loads %s, searches %s: a darwin tree's run paths are the system's or relative to the image", lib, elsewhere)
			case strings.HasPrefix(lib, "@rpath/") && relative:
			default:
				return fmt.Errorf("loads %s: a darwin tree binds to the system's libraries or its own alone", lib)
			}
		}
	}
	return nil
}

// machO reports whether a file bears a Mach-O magic: a thin file's
// in either byte order, or the universal file's followed by its
// architecture count — a Java class file bears the same magic
// followed by its version, 45 at the earliest, where a universal
// file holds a handful of architectures, which is how file(1) tells
// them apart. A file shorter than the magic and count reads as
// zeros past its end: no magic, or a universal file of no
// architectures, which the reader refuses.
func machO(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var head [8]byte
	if _, err := io.ReadFull(f, head[:]); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, err
	}
	switch binary.BigEndian.Uint32(head[:4]) {
	case macho.Magic32, macho.Magic64, 0xcefaedfe, 0xcffaedfe:
		return true, nil
	case macho.MagicFat:
		return binary.BigEndian.Uint32(head[4:]) < 45, nil
	}
	return false, nil
}

// openMachO opens a Mach-O file's architectures, every one of a
// universal file, with what closes them.
func openMachO(path string) ([]*macho.File, func(), error) {
	if fat, err := macho.OpenFat(path); err == nil {
		var files []*macho.File
		for _, a := range fat.Arches {
			files = append(files, a.File)
		}
		return files, func() { fat.Close() }, nil
	}
	f, err := macho.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("no Mach-O executable: %w", err)
	}
	return []*macho.File{f}, func() { f.Close() }, nil
}

// imageRelative reports whether a binding or run path resolves
// within the image: through the loading file's own location
// (loaderAt, its directory within the tree) or the process's
// executable's (execAt), the location itself or a path under it
// that does not climb past the tree's root, where it would reach
// the runner's own files and resolve there alone.
func imageRelative(p, loaderAt, execAt string) bool {
	for anchor, at := range map[string]string{"@executable_path": execAt, "@loader_path": loaderAt} {
		if p == anchor {
			return true
		}
		if rest, ok := strings.CutPrefix(p, anchor+"/"); ok {
			within := path.Clean(path.Join(at, rest))
			return within != ".." && !strings.HasPrefix(within, "../")
		}
	}
	return false
}

// substrate reports whether a path lies in the darwin row's
// execution substrate, the system's libraries and frameworks.
func substrate(p string) bool {
	return strings.HasPrefix(p, "/usr/lib/") || strings.HasPrefix(p, "/System/Library/")
}

// dylibCommands are the load commands that bind a library: the
// plain one debug/macho reads for us and the weak, re-exported,
// lazy and upward ones it leaves as bytes, all of one layout, the
// library's name at the offset the command's third word gives; an
// rpath command names a run path the same way.
var dylibCommands = map[macho.LoadCmd]bool{
	macho.LoadCmdDylib: true,
	0x80000018:         true, // LC_LOAD_WEAK_DYLIB
	0x8000001f:         true, // LC_REEXPORT_DYLIB
	0x20:               true, // LC_LAZY_LOAD_DYLIB
	0x80000023:         true, // LC_LOAD_UPWARD_DYLIB
}

// rpathCommand names a run path (LC_RPATH), its path at the offset
// the command's third word gives, past its twelve fixed bytes;
// dyldEnvironmentCommand (LC_DYLD_ENVIRONMENT) sets a loader
// variable, which the darwin row refuses outright.
const (
	rpathCommand           macho.LoadCmd = 0x8000001c
	dyldEnvironmentCommand macho.LoadCmd = 0x27
)

// loadedLibraries names every library a Mach-O file's load commands
// bind, weakly or not, and every run path they name; a binding or
// run path whose name cannot be read — the command too short, or
// the name's offset outside it or inside its fixed words — is
// refused, an unknown library being no library the gate can admit.
func loadedLibraries(f *macho.File) (libs, rpaths []string, err error) {
	for _, l := range f.Loads {
		if d, ok := l.(*macho.Dylib); ok {
			libs = append(libs, d.Name)
			continue
		}
		if r, ok := l.(*macho.Rpath); ok {
			rpaths = append(rpaths, r.Path)
			continue
		}
		raw := l.Raw()
		if len(raw) < 8 {
			continue
		}
		cmd := macho.LoadCmd(f.ByteOrder.Uint32(raw))
		if cmd == dyldEnvironmentCommand {
			return nil, nil, errors.New("sets a loader environment: a darwin tree sets none")
		}
		fixed := 24
		if cmd == rpathCommand {
			fixed = 12
		} else if !dylibCommands[cmd] {
			continue
		}
		if len(raw) < fixed {
			return nil, nil, fmt.Errorf("a library binding (command %#x) names no readable library", uint32(cmd))
		}
		off := int(f.ByteOrder.Uint32(raw[8:]))
		if off < fixed || off >= len(raw) {
			return nil, nil, fmt.Errorf("a library binding (command %#x) names no readable library", uint32(cmd))
		}
		name := raw[off:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		if cmd == rpathCommand {
			rpaths = append(rpaths, string(name))
		} else {
			libs = append(libs, string(name))
		}
	}
	return libs, rpaths, nil
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
