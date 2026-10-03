// Package recipe builds a plugin's platform trees: for each platform
// asked, a directory holding the entrypoint executable at its root,
// laid out as out/<os>-<arch>/<entrypoint>, the shape `pb plugin
// build` packages. Each kind of the catalog has its builder here; a
// native kind builds only the host's platform, the others every
// platform asked from one host.
package recipe

import (
	"bytes"
	"context"
	"debug/elf"
	"debug/macho"
	"errors"
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
	catalog.KindNode: func(ctx context.Context, _ *catalog.Catalog, _ string, p *catalog.Plugin, version string, platforms []string, out string) error {
		return buildNode(ctx, p, version, platforms, out)
	},
	catalog.KindRelease: func(ctx context.Context, _ *catalog.Catalog, _ string, p *catalog.Plugin, version string, platforms []string, out string) error {
		return buildRelease(ctx, p, version, platforms, out)
	},
	catalog.KindBazel: buildBazel,
	catalog.KindRust:  buildRust,
	catalog.KindSwift: buildSwift,
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

// layDown lays an executable a native kind built down as the
// platform's entrypoint once it is held to load elsewhere than the
// runner that built it: on linux static, for a kind that takes no
// base (the OS sandbox row loads a static entrypoint alone, and a
// baseless image carries no loader; a kind over the base links the
// C library and is not held), on darwin bound to the system's
// libraries alone (a toolchain's library reached through a search
// path is absent on every other machine).
func layDown(kind catalog.Kind, name, platform, built, dst string) error {
	os, _ := catalog.SplitPlatform(platform)
	var err error
	switch os {
	case "linux":
		if !kind.NeedsBase() {
			err = checkStatic(built)
		}
	case "darwin":
		err = checkPortable(built)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, platform, err)
	}
	return copyFile(built, dst)
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
// (`/usr/lib`, `/System`): one named through `@rpath` or an
// absolute path into a toolchain loads on the machine that built it
// alone. Every load command naming a library counts — the weak ones
// too, which is how a toolchain's compatibility library is bound.
// The rule is no search-path binding at all, not "unresolvable": a
// back-deployed runtime library bound through `@rpath` with
// `/usr/lib/swift` among the search paths would load on a recent
// macOS, and is refused here all the same; the remedy is a
// deployment target the system's runtime serves, not a looser rule.
func checkPortable(path string) error {
	var files []*macho.File
	if fat, err := macho.OpenFat(path); err == nil {
		defer fat.Close()
		for _, a := range fat.Arches {
			files = append(files, a.File)
		}
	} else {
		f, err := macho.Open(path)
		if err != nil {
			return fmt.Errorf("no Mach-O executable: %w", err)
		}
		defer f.Close()
		files = append(files, f)
	}
	for _, f := range files {
		libs, err := loadedLibraries(f)
		if err != nil {
			return err
		}
		for _, lib := range libs {
			if !strings.HasPrefix(lib, "/usr/lib/") && !strings.HasPrefix(lib, "/System/") {
				return fmt.Errorf("loads %s: a darwin tree binds to the system's libraries alone", lib)
			}
		}
	}
	return nil
}

// dylibCommands are the load commands that bind a library: the
// plain one debug/macho reads for us and the weak, re-exported,
// lazy and upward ones it leaves as bytes, all of one layout, the
// library's name at the offset the command's third word gives.
var dylibCommands = map[macho.LoadCmd]bool{
	macho.LoadCmdDylib: true,
	0x80000018:         true, // LC_LOAD_WEAK_DYLIB
	0x8000001f:         true, // LC_REEXPORT_DYLIB
	0x20:               true, // LC_LAZY_LOAD_DYLIB
	0x80000023:         true, // LC_LOAD_UPWARD_DYLIB
}

// loadedLibraries names every library a Mach-O file's load commands
// bind, weakly or not; a binding whose name cannot be read — the
// command too short, or the name's offset outside it or inside its
// fixed words — is refused, an unknown library being no library the
// gate can admit.
func loadedLibraries(f *macho.File) ([]string, error) {
	var libs []string
	for _, l := range f.Loads {
		if d, ok := l.(*macho.Dylib); ok {
			libs = append(libs, d.Name)
			continue
		}
		raw := l.Raw()
		if len(raw) < 8 {
			continue
		}
		cmd := macho.LoadCmd(f.ByteOrder.Uint32(raw))
		if !dylibCommands[cmd] {
			continue
		}
		if len(raw) < 24 {
			return nil, fmt.Errorf("a library binding (command %#x) names no readable library", uint32(cmd))
		}
		off := int(f.ByteOrder.Uint32(raw[8:]))
		if off < 24 || off >= len(raw) {
			return nil, fmt.Errorf("a library binding (command %#x) names no readable library", uint32(cmd))
		}
		name := raw[off:]
		if i := bytes.IndexByte(name, 0); i >= 0 {
			name = name[:i]
		}
		libs = append(libs, string(name))
	}
	return libs, nil
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
