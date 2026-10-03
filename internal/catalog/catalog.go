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
	"reflect"
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
	// KindRelease takes an executable upstream ships prebuilt, from a
	// GitHub release's asset or a URL: an archive holding it, or the
	// executable itself.
	KindRelease Kind = "release"
	// KindBazel builds a C++ target with bazel from an upstream source
	// archive, on a runner of the platform itself.
	KindBazel Kind = "bazel"
	// KindRust installs a crate's executable with cargo, on a runner
	// of the platform itself.
	KindRust Kind = "rust"
	// KindSwift builds a SwiftPM product at the repository's tag on
	// a runner of the platform itself.
	KindSwift Kind = "swift"
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
	// Parameter is the plugin parameter the probe's request carries,
	// for a plugin that answers nothing without one (an option naming
	// where its generated code's types live).
	Parameter string `yaml:"parameter"`
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
	// Product is the SwiftPM product a swift recipe builds, an
	// executable of the package at the repository's tag.
	Product string `yaml:"product"`
	// Assets map a platform to the asset holding the executable: a
	// release asset's name under the repository's release, or a URL
	// (`https://...`) wherever upstream publishes — Maven Central, a
	// project's binary host; Member is the executable's path within
	// the archive, none where the asset is the executable itself
	// (`{exe}` is `.exe` on windows, empty elsewhere).
	Assets map[string]string `yaml:"assets"`
	Member string            `yaml:"member"`
	// Members name a platform's member where it differs from Member
	// (upstream laying its archives out per platform).
	Members map[string]string `yaml:"members"`
	// Maven names a release recipe's Maven Central artifact,
	// `group:artifact`, whose metadata lists its versions; Npm the npm
	// package whose registry entry lists them; a recipe naming
	// neither takes the repository's releases.
	Maven string `yaml:"maven"`
	Npm   string `yaml:"npm"`
	// Checksum names the digest upstream publishes beside each asset
	// (`sha256`: the asset's URL with `.sha256` appended, the hex
	// digest first on its line), verified before the asset is read.
	Checksum string `yaml:"checksum"`

	// Crate names a rust recipe's package on crates.io, whose
	// versions are the recipe's; Bin the executable the crate
	// installs, where it is not the entrypoint's name.
	Crate string `yaml:"crate"`
	Bin   string `yaml:"bin"`

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
	// Toolchains pin, by kind, the toolchain the pipeline installs
	// before building a kind's trees (`rust`: the rust release;
	// `swift`: the Swift release), each one exact release, three
	// components, so neither installer floats to a later patch.
	Toolchains map[string]string `yaml:"toolchains"`
	// SDKs pin, by kind, the sha256 of the SDK the pipeline installs
	// beside the toolchain on linux (`swift`: the release's static
	// Linux SDK bundle), held equal to the checksum swift.org
	// publishes before it is installed.
	SDKs map[string]string `yaml:"sdks"`
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
	mavenRE      = regexp.MustCompile(`^[A-Za-z0-9._-]+:[A-Za-z0-9._-]+$`)
	crateRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
	toolchainRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	checksumRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// owned is the recipe fields each kind may set, by the fields'
// YAML keys — the common ones every kind's — the one table the
// foreign-field refusal reads, so a new kind or field is named once.
var owned = map[Kind]map[string]bool{
	KindGo:      fields("platforms", "module", "dir", "package", "tags", "repository", "tag"),
	KindNode:    fields("platforms", "package"),
	KindRelease: fields("repository", "tag", "assets", "member", "members", "maven", "npm", "checksum"),
	KindBazel:   fields("platforms", "repository", "tag", "archive", "strip", "files", "target", "output", "options"),
	KindRust:    fields("platforms", "crate", "bin"),
	KindSwift:   fields("platforms", "repository", "tag", "product"),
}

// pinned are the kinds whose toolchain the pipeline installs from
// the catalog's `toolchains`, each required where a recipe of the
// kind exists.
var pinned = map[Kind]bool{KindRust: true, KindSwift: true}

// sdkPinned are the kinds whose linux SDK the pipeline installs from
// the catalog's `sdks`, required where a recipe of the kind exists.
var sdkPinned = map[Kind]bool{KindSwift: true}

// unserved are the platforms a kind has no runner path for: no
// toolchain the pipeline installs builds a swift package on windows,
// and no bazel C++ toolchain is established for windows/arm64.
var unserved = map[Kind]map[string]bool{
	KindSwift: {"windows/amd64": true, "windows/arm64": true},
	KindBazel: {"windows/arm64": true},
}

// common are the fields every kind takes; a release recipe's
// platforms are its assets' keys, so it takes no `platforms`.
var common = []string{"source", "kind", "entrypoint", "silent", "frozen", "parameter"}

func fields(names ...string) map[string]bool {
	set := map[string]bool{}
	for _, n := range append(names, common...) {
		set[n] = true
	}
	return set
}

// setFields names the recipe fields the entry sets, by their YAML
// keys, in the struct's order.
func (p *Plugin) setFields() []string {
	var out []string
	v := reflect.ValueOf(*p)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		key, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if key == "" || key == "-" {
			continue
		}
		if !v.Field(i).IsZero() {
			out = append(out, key)
		}
	}
	return out
}

// MemberOf is the member of a platform's asset: the platform's own
// where Members names one, else Member.
func (p *Plugin) MemberOf(platform string) string {
	if m, ok := p.Members[platform]; ok {
		return m
	}
	return p.Member
}

// Archive tells, by an asset's name, the archive the member is
// extracted from — "zip", or "tgz" for a gzip-compressed tar — or
// "" for an asset that is the executable itself.
func Archive(asset string) string {
	switch {
	case strings.HasSuffix(asset, ".zip"):
		return "zip"
	case strings.HasSuffix(asset, ".tar.gz"), strings.HasSuffix(asset, ".tgz"):
		return "tgz"
	}
	return ""
}

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
	for kind, v := range c.Toolchains {
		if !pinned[Kind(kind)] {
			errs = append(errs, fmt.Errorf("toolchains: %q names no kind the pipeline pins a toolchain for", kind))
		}
		if !toolchainRE.MatchString(v) {
			errs = append(errs, fmt.Errorf("toolchains: %s %q is no release number", kind, v))
		}
	}
	for kind, v := range c.SDKs {
		if !sdkPinned[Kind(kind)] {
			errs = append(errs, fmt.Errorf("sdks: %q names no kind the pipeline installs an SDK for", kind))
		}
		if !checksumRE.MatchString(v) {
			errs = append(errs, fmt.Errorf("sdks: %s %q is no sha256", kind, v))
		}
	}
	kinds := map[Kind]bool{}
	for _, p := range c.Plugins {
		kinds[p.Kind] = true
	}
	for kind := range pinned {
		if kinds[kind] && c.Toolchains[string(kind)] == "" {
			errs = append(errs, fmt.Errorf("toolchains: a %s recipe needs the %s toolchain pinned", kind, kind))
		}
	}
	for kind := range sdkPinned {
		if kinds[kind] && c.SDKs[string(kind)] == "" {
			errs = append(errs, fmt.Errorf("sdks: a %s recipe needs the %s SDK's checksum pinned", kind, kind))
		}
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
		case KindNode:
			if p.Package == "" {
				fail(name, "node: package required")
			}
		case KindRelease:
			if len(p.Assets) == 0 {
				fail(name, "release: assets required")
			}
			sources := 0
			for _, set := range []bool{p.Repository != "", p.Maven != "", p.Npm != ""} {
				if set {
					sources++
				}
			}
			if sources != 1 {
				fail(name, "release: exactly one of repository, maven and npm names where the versions are")
			}
			if (p.Repository != "") != (p.Tag != "") {
				fail(name, "release: repository and tag go together")
			}
			if p.Repository != "" && (!repoRE.MatchString(p.Repository) || !strings.Contains(p.Tag, "{version}")) {
				fail(name, "release: repository owner/name and tag with {version} required")
			}
			if p.Maven != "" && !mavenRE.MatchString(p.Maven) {
				fail(name, "release: maven %q is no group:artifact", p.Maven)
			}
			if p.Checksum != "" && p.Checksum != "sha256" {
				fail(name, "release: checksum %q is none of sha256", p.Checksum)
			}
			for _, asset := range p.Assets {
				byURL := strings.HasPrefix(asset, "https://")
				// A release asset's name may be the same at every
				// release, the tag naming the version; a URL names it
				// itself.
				if byURL && !strings.Contains(asset, "{version}") && !strings.Contains(asset, "{tag}") {
					fail(name, "release: asset %q names neither {version} nor {tag}", asset)
				}
				if !byURL && p.Repository == "" {
					fail(name, "release: asset %q is a release asset's name, which needs the repository", asset)
				}
				if !byURL && strings.ContainsAny(asset, "/:") {
					fail(name, "release: asset %q is neither a release asset's bare name nor an https URL", asset)
				}
			}
			for pl := range p.Members {
				if _, ok := p.Assets[pl]; !ok {
					fail(name, "release: members names %q, which has no asset", pl)
				}
			}
			for pl, asset := range p.Assets {
				member := p.MemberOf(pl)
				if member == "" && Archive(asset) != "" {
					fail(name, "release: asset %q is an archive, which needs the member", asset)
				}
				if member != "" && Archive(asset) == "" {
					fail(name, "release: asset %q is no archive, the executable itself, which takes no member", asset)
				}
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
		case KindSwift:
			if !repoRE.MatchString(p.Repository) || !strings.Contains(p.Tag, "{version}") || !entrypointRE.MatchString(p.Product) {
				fail(name, "swift: repository, tag with {version} and product (a bare name) required")
			}
		case KindRust:
			if !crateRE.MatchString(p.Crate) {
				fail(name, "rust: crate required, a crates.io package name (a letter first, 64 at most)")
			}
			if p.Bin != "" && !entrypointRE.MatchString(p.Bin) {
				fail(name, "rust: bin %q is no bare name", p.Bin)
			}
		default:
			fail(name, "unknown kind %q", p.Kind)
		}
		for _, field := range p.setFields() {
			if !owned[p.Kind][field] {
				fail(name, "%s: a field of another kind set (%s)", p.Kind, field)
			}
		}
		if len(platforms) == 0 {
			fail(name, "no platforms")
		}
		seen := map[string]bool{}
		for _, pl := range platforms {
			if !known(pl) {
				fail(name, "platform %q is none of %v", pl, Platforms)
			}
			if unserved[p.Kind][pl] {
				fail(name, "%s: no runner path builds the kind for %s", p.Kind, pl)
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
func (k Kind) Native() bool { return k == KindBazel || k == KindRust || k == KindSwift }

// Known reports whether the kind is one the catalog defines.
func (k Kind) Known() bool { _, ok := owned[k]; return ok }

// NeedsBase reports whether the kind's Linux trees are layered over
// the catalog's base: every kind whose executables link the C
// library, which is every kind but go, rust and swift, whose Linux
// executables are static.
func (k Kind) NeedsBase() bool { return k != KindGo && k != KindRust && k != KindSwift }

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
