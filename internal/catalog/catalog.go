// Package catalog reads the plugin catalog: catalog.yaml, the index of
// plugins and their recipes, and plugins/<name>/versions, the tags
// published per plugin. The two are separate files so a version bump
// is one appended line, reviewable in a pull request as such, while
// the recipes stay hand-written.
package catalog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/google/go-containerregistry/pkg/name"
	"golang.org/x/mod/semver"
)

// Kind is a recipe kind: how a plugin's platform trees are produced.
type Kind string

const (
	// KindGo cross-compiles a Go main package with CGO disabled.
	KindGo Kind = "go"
	// KindNode compiles an npm package's executable with bun into a
	// standalone executable per platform.
	KindNode Kind = "node"
	// KindRelease takes an executable an upstream GitHub release
	// ships prebuilt, one asset per platform.
	KindRelease Kind = "release"
	// KindBazel builds a C++ target with bazel from an upstream source
	// archive, on a runner of the platform itself.
	KindBazel Kind = "bazel"
)

// Platforms is every platform a recipe may serve, in the spelling
// order pb's platform term has (`os/arch`, Go's names).
var Platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// Plugin is one catalog entry.
type Plugin struct {
	// Source is the upstream project's URL, for readers.
	Source string `yaml:"source"`
	Kind   Kind   `yaml:"kind"`
	// Entrypoint is the executable's name at the tree's root, the
	// image's `/<entrypoint>`, on every platform.
	Entrypoint string `yaml:"entrypoint"`
	// Platforms are the platforms the plugin serves. A release recipe
	// serves the keys of Assets instead.
	Platforms []string `yaml:"platforms"`
	// Silent says the plugin generates only for options the probe's
	// file lacks, so a response holding no file is its right answer
	// to the probe (recipe.Probe); every other plugin answers with a
	// file.
	Silent bool `yaml:"silent"`
	// Frozen says the versions file is complete: the bump appends
	// nothing, upstream's later versions carrying the executable no
	// more.
	Frozen bool `yaml:"frozen"`

	// Module and Package name a go recipe's main package: the module
	// the versions belong to and the package's path within it (`.`
	// for the module's root). A go recipe built from a repository
	// tag names no module: Dir is the module's directory in the
	// repository (`.` for its root), and the module's path is read
	// from the go.mod there at the tag's commit — upstream's own
	// fact, a nested module's major suffix among it.
	Module  string `yaml:"module"`
	Dir     string `yaml:"dir"`
	Package string `yaml:"package"`
	// Tags are a go recipe's build tags.
	Tags []string `yaml:"tags"`

	// Repository is the GitHub repository of a release or bazel
	// recipe, `owner/name`, or of a go recipe whose versions are the
	// repository's releases rather than the module proxy's list — a
	// tag the proxy does not list, fetched at the tag's commit; Tag
	// spells the release tag from a version (`{version}` the tag
	// with pb's `v` stripped).
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	// Assets map a platform to the release asset holding the
	// executable and Member is its path within the archive
	// (`{exe}` is `.exe` on windows, empty elsewhere).
	Assets map[string]string `yaml:"assets"`
	Member string            `yaml:"member"`

	// Archive is a bazel recipe's source archive URL, Strip the
	// leading path components dropped extracting it, Files the
	// directory under the source tree the recipe's own files
	// (plugins/<name>/files) are copied to, Target the bazel target and
	// Output the built executable's path under the source tree.
	Archive string `yaml:"archive"`
	Strip   int    `yaml:"strip"`
	Files   string `yaml:"files"`
	Target  string `yaml:"target"`
	Output  string `yaml:"output"`
	// Options are a bazel recipe's options per operating system
	// (`linux`, `darwin`, `windows`): what a platform's build needs
	// beyond the source's own rc files, or in place of them.
	Options map[string]BazelOptions `yaml:"options"`

	// Versions are the tags published, ascending, read from
	// plugins/<name>/versions.
	Versions []string `yaml:"-"`
}

// BazelOptions are bazel's startup options and build options for one
// operating system.
type BazelOptions struct {
	Startup []string `yaml:"startup"`
	Build   []string `yaml:"build"`
}

// Catalog is the catalog read whole.
type Catalog struct {
	// Registry is the repository prefix every plugin publishes under.
	Registry string `yaml:"registry"`
	// Base is the image, by index digest, the Linux trees of a node,
	// release or bazel recipe are layered over.
	Base string `yaml:"base"`
	// Plugins by name, buf's `owner/plugin`.
	Plugins map[string]*Plugin `yaml:"plugins"`
	// Dir is the catalog's directory, where plugins/ lies.
	Dir string `yaml:"-"`
}

// Load reads the catalog at dir and every plugin's versions file,
// validated (Validate).
func Load(dir string) (*Catalog, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "catalog.yaml"))
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := yaml.UnmarshalWithOptions(raw, &c, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("catalog.yaml: %w", err)
	}
	c.Dir = dir
	for name, p := range c.Plugins {
		vs, err := ReadVersions(c.VersionsPath(name))
		if err != nil {
			return nil, err
		}
		p.Versions = vs
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// VersionsPath is the versions file of the plugin named.
func (c *Catalog) VersionsPath(name string) string {
	return filepath.Join(c.Dir, "plugins", filepath.FromSlash(name), "versions")
}

// ReadVersions reads a versions file: one tag per line, blank lines
// and `#` comments ignored.
func ReadVersions(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vs []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		vs = append(vs, line)
	}
	return vs, nil
}

// AppendVersions appends tags to a versions file, one per line.
func AppendVersions(path string, tags []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, t := range tags {
		if _, err := fmt.Fprintln(f, t); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

var (
	nameRE       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*/[a-z0-9]+(-[a-z0-9]+)*$`)
	versionRE    = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?$`)
	entrypointRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	repoRE       = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
)

// Names are the plugin names, sorted.
func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.Plugins))
	for n := range c.Plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Reference is the image reference of a plugin at a version.
func (c *Catalog) Reference(name, version string) string {
	return c.Registry + "/" + name + ":" + version
}

// Validate refuses a catalog with a malformed entry: a name not
// `owner/plugin`, an unknown kind, a kind missing its fields or
// carrying another's, a platform outside Platforms or repeated, an
// entrypoint that is no bare file name, a version that is none
// (IsVersion) or out of ascending order.
func (c *Catalog) Validate() error {
	var errs []error
	fail := func(name, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{name}, args...)...))
	}
	if _, err := name.NewRepository(c.Registry + "/x/y"); err != nil || strings.HasSuffix(c.Registry, "/") || strings.Contains(c.Registry, "@") || strings.ContainsAny(c.Registry[strings.LastIndex(c.Registry, "/")+1:], ":") {
		errs = append(errs, fmt.Errorf("registry: %q is no repository prefix", c.Registry))
	}
	if !strings.Contains(c.Base, "@sha256:") {
		errs = append(errs, fmt.Errorf("base: %q names no digest", c.Base))
	}
	if len(c.Plugins) == 0 {
		errs = append(errs, errors.New("no plugins"))
	}
	for _, name := range c.Names() {
		p := c.Plugins[name]
		if !nameRE.MatchString(name) {
			fail(name, "not owner/plugin")
		}
		if p.Source == "" {
			fail(name, "no source")
		}
		if !entrypointRE.MatchString(p.Entrypoint) || strings.HasSuffix(p.Entrypoint, ".exe") {
			fail(name, "entrypoint %q is no bare name", p.Entrypoint)
		}
		platforms := p.Platforms
		switch p.Kind {
		case KindGo:
			if p.Package == "" {
				fail(name, "go: package required")
			}
			if (p.Repository != "") != (p.Tag != "") || (p.Repository != "") != (p.Dir != "") {
				fail(name, "go: repository, tag and dir go together")
			}
			if p.Repository != "" {
				if !repoRE.MatchString(p.Repository) || !strings.Contains(p.Tag, "{version}") {
					fail(name, "go: repository owner/name and tag with {version} required")
				}
				if p.Module != "" {
					fail(name, "go: the module is read from the repository's go.mod, not named")
				}
				// A slash-separated, clean, relative path, as the API
				// addresses the repository's tree, the same on every host.
				if !fs.ValidPath(p.Dir) || strings.Contains(p.Dir, "\\") {
					fail(name, "go: dir %q is no relative directory", p.Dir)
				}
			} else if p.Module == "" {
				fail(name, "go: module required")
			}
			if p.Assets != nil || p.Member != "" || p.Archive != "" || p.Files != "" || p.Target != "" || p.Output != "" || p.Options != nil {
				fail(name, "go: a field of another kind set")
			}
		case KindNode:
			if p.Package == "" {
				fail(name, "node: package required")
			}
			if p.Module != "" || p.Dir != "" || p.Tags != nil || p.Repository != "" || p.Tag != "" || p.Assets != nil || p.Member != "" || p.Archive != "" || p.Files != "" || p.Target != "" || p.Output != "" || p.Options != nil {
				fail(name, "node: a field of another kind set")
			}
		case KindRelease:
			if !repoRE.MatchString(p.Repository) || !strings.Contains(p.Tag, "{version}") || len(p.Assets) == 0 || p.Member == "" {
				fail(name, "release: repository, tag with {version}, assets and member required")
			}
			if p.Platforms != nil || p.Module != "" || p.Dir != "" || p.Package != "" || p.Tags != nil || p.Archive != "" || p.Files != "" || p.Target != "" || p.Output != "" || p.Options != nil {
				fail(name, "release: a field of another kind set (platforms are the assets' keys)")
			}
			platforms = make([]string, 0, len(p.Assets))
			for pl := range p.Assets {
				platforms = append(platforms, pl)
			}
			sort.Strings(platforms)
		case KindBazel:
			if !repoRE.MatchString(p.Repository) || !strings.Contains(p.Tag, "{version}") || !strings.Contains(p.Archive, "{version}") || p.Target == "" || p.Output == "" {
				fail(name, "bazel: repository, tag and archive with {version}, target and output required")
			}
			if p.Module != "" || p.Dir != "" || p.Package != "" || p.Tags != nil || p.Assets != nil || p.Member != "" {
				fail(name, "bazel: a field of another kind set")
			}
			if p.Files != "" && (filepath.IsAbs(p.Files) || strings.Contains(p.Files, "..") || strings.Contains(p.Files, "/")) {
				fail(name, "bazel: files %q is no bare directory name", p.Files)
			}
			if p.Strip < 0 {
				fail(name, "bazel: strip negative")
			}
			for os := range p.Options {
				if os != "linux" && os != "darwin" && os != "windows" {
					fail(name, "bazel: options for %q, no operating system", os)
				}
			}
		default:
			fail(name, "unknown kind %q", p.Kind)
		}
		if len(platforms) == 0 {
			fail(name, "no platforms")
		}
		seen := map[string]bool{}
		for _, pl := range platforms {
			if !known(pl) {
				fail(name, "platform %q is none of %v", pl, Platforms)
			}
			if seen[pl] {
				fail(name, "platform %q twice", pl)
			}
			seen[pl] = true
		}
		if p.Frozen && len(p.Versions) == 0 {
			fail(name, "frozen with no version: a versions file is complete only holding one")
		}
		for i, v := range p.Versions {
			if !IsVersion(v) {
				fail(name, "version %q is no vMAJOR.MINOR[.PATCH]", v)
				continue
			}
			if i > 0 && semver.Compare(p.Versions[i-1], v) >= 0 {
				fail(name, "version %s follows %s, not ascending", v, p.Versions[i-1])
			}
		}
	}
	return errors.Join(errs...)
}

// IsVersion reports whether v is a version as the catalog spells
// tags, buf's spelling of upstream's release: `v` and two or three
// numeric components, no prerelease or build suffix — `v1.36.12`,
// and `v36.2` for a project whose releases carry two components, as
// protobuf's do.
func IsVersion(v string) bool { return versionRE.MatchString(v) }

func known(platform string) bool {
	for _, p := range Platforms {
		if p == platform {
			return true
		}
	}
	return false
}

// PlatformsOf are the platforms a plugin serves in Platforms' order:
// the entry's own, or a release recipe's asset keys.
func (p *Plugin) PlatformsOf() []string {
	set := map[string]bool{}
	for _, pl := range p.Platforms {
		set[pl] = true
	}
	for pl := range p.Assets {
		set[pl] = true
	}
	var out []string
	for _, pl := range Platforms {
		if set[pl] {
			out = append(out, pl)
		}
	}
	return out
}

// Native reports whether the kind builds on the platform itself, one
// runner per platform, rather than cross-building every platform on
// one host.
func (k Kind) Native() bool { return k == KindBazel }

// NeedsBase reports whether the kind's Linux trees are layered over
// the catalog's base: every kind whose executables link the C
// library, which is every kind but go.
func (k Kind) NeedsBase() bool { return k != KindGo }

// Expand fills a recipe template: `{version}` with the version's
// number (the tag less its `v`), `{tag}` with the tag as pb spells
// it, and `{exe}` with `.exe` on windows and nothing elsewhere.
func Expand(template, version, platform string) string {
	exe := ""
	if strings.HasPrefix(platform, "windows/") {
		exe = ".exe"
	}
	r := strings.NewReplacer("{version}", strings.TrimPrefix(version, "v"), "{tag}", version, "{exe}", exe)
	return r.Replace(template)
}

// SplitPlatform splits `os/arch`.
func SplitPlatform(platform string) (os, arch string) {
	os, arch, _ = strings.Cut(platform, "/")
	return os, arch
}
