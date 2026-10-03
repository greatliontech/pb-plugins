package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository's own catalog loads and validates: every plugin
// serves at least one platform, its versions are ascending, and the
// names are buf's.
func TestRepositoryCatalogLoads(t *testing.T) {
	c, err := Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range c.Names() {
		p := c.Plugins[name]
		if len(p.PlatformsOf()) == 0 || len(p.Versions) == 0 {
			t.Errorf("%s: no platforms or versions", name)
		}
	}
	if got := c.Reference("protocolbuffers/go", "v1.36.12"); got != "ghcr.io/greatliontech/pb-plugins/protocolbuffers/go:v1.36.12" {
		t.Errorf("reference %s", got)
	}
}

func write(t *testing.T, dir, catalogYAML string, versions map[string]string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(catalogYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, vs := range versions {
		p := filepath.Join(dir, "plugins", filepath.FromSlash(name))
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "versions"), []byte(vs), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const head = "registry: r.example/p\nbase: r.example/base@sha256:" + "ab" + "\nplugins:\n"

// Validate refuses each malformed shape naming the plugin and the
// fault, and admits the well-formed ones, two-component versions
// included.
func TestValidate(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		versions string
		want     string
	}{
		{"go ok", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\nv36.2\n", ""},
		{"bad name", "  ab:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "not owner/plugin"},
		{"unknown kind", "  a/b:\n    source: s\n    kind: rust\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "unknown kind"},
		{"go tags, frozen, silent", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    tags: [t]\n    entrypoint: e\n    platforms: [linux/amd64]\n    silent: true\n    frozen: true\n", "v1.0.0\n", ""},
		{"frozen, no version", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n    frozen: true\n", "", "frozen with no version"},
		{"node tags", "  a/b:\n    source: s\n    kind: node\n    package: p\n    tags: [t]\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"foreign field", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    target: t\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"platform unknown", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/386]\n", "v1.0.0\n", "is none of"},
		{"platform twice", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64, linux/amd64]\n", "v1.0.0\n", "twice"},
		{"no platforms", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n", "v1.0.0\n", "no platforms"},
		{"entrypoint path", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: bin/e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare name"},
		{"version prerelease", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0-rc1\n", "is no vMAJOR"},
		{"version order", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.1.0\nv1.0.0\n", "not ascending"},
		{"version twice", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\nv1.0.0\n", "not ascending"},
		{"release ok", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    member: bin/e{exe}\n    entrypoint: e\n    assets:\n      linux/amd64: x.tar.gz\n      windows/amd64: x.zip\n", "v1.0.0\n", ""},
		{"release from maven", "  a/b:\n    source: s\n    kind: release\n    maven: g.h:a\n    checksum: sha256\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}/a-{version}.exe\n", "v1.0.0\n", ""},
		{"release from npm", "  a/b:\n    source: s\n    kind: release\n    npm: p\n    member: bin/e{exe}\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/v{version}/linux-x64.tar.gz\n", "v1.0.0\n", ""},
		{"release two sources", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    maven: g:a\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "exactly one of"},
		{"release no source", "  a/b:\n    source: s\n    kind: release\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "exactly one of"},
		{"release name without repository", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    entrypoint: e\n    assets:\n      linux/amd64: a-{version}.exe\n", "v1.0.0\n", "needs the repository"},
		{"release archive without member", "  a/b:\n    source: s\n    kind: release\n    npm: p\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.tar.gz\n", "v1.0.0\n", "needs the member"},
		{"release executable with member", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    member: bin/e\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "takes no member"},
		{"release asset without version", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/latest.exe\n", "v1.0.0\n", "names neither {version} nor {tag}"},
		{"release checksum unknown", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    checksum: md5\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "none of sha256"},
		{"release tag without repository", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    tag: v{version}\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "repository and tag go together"},
		{"release name with a path", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    entrypoint: e\n    assets:\n      linux/amd64: http://h/{version}.exe\n", "v1.0.0\n", "neither a release asset's bare name"},
		{"release repository beside URL assets", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    entrypoint: e\n    assets:\n      linux/amd64: https://h/{version}.exe\n", "v1.0.0\n", ""},
		{"release members", "  a/b:\n    source: s\n    kind: release\n    npm: p\n    member: bin/e\n    members:\n      darwin/amd64: x64/e\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/v{version}/l.tar.gz\n      darwin/amd64: https://x/v{version}/d.tar.gz\n", "v1.0.0\n", ""},
		{"release members without asset", "  a/b:\n    source: s\n    kind: release\n    npm: p\n    member: bin/e\n    members:\n      darwin/arm64: x64/e\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/v{version}/l.tar.gz\n", "v1.0.0\n", "has no asset"},
		{"release members for an executable", "  a/b:\n    source: s\n    kind: release\n    maven: g:a\n    members:\n      linux/amd64: x\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "takes no member"},
		{"release maven malformed", "  a/b:\n    source: s\n    kind: release\n    maven: g/a\n    entrypoint: e\n    assets:\n      linux/amd64: https://x/{version}.exe\n", "v1.0.0\n", "no group:artifact"},
		{"go with maven", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    maven: g:a\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"release with platforms", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    member: bin/e\n    entrypoint: e\n    platforms: [linux/amd64]\n    assets:\n      linux/amd64: x.tar.gz\n", "v1.0.0\n", "a field of another kind"},
		{"bazel ok", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    strip: 1\n    files: plugins\n    target: //t\n    output: bazel-bin/t{exe}\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"bazel files path", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    files: ../plugins\n    target: //t\n    output: bazel-bin/t\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare directory"},
		{"bazel options os", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    target: //t\n    output: bazel-bin/t\n    entrypoint: e\n    platforms: [linux/amd64]\n    options:\n      freebsd:\n        build: [--x]\n", "v1.0.0\n", "no operating system"},
		{"go from a tag", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    dir: sub/mod\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"go from a tag at the root", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: \"{version}\"\n    dir: .\n    package: cmd/e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"go tag alone", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    tag: v{version}\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "repository, tag and dir go together"},
		{"go tag without dir", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "repository, tag and dir go together"},
		{"go tag without version", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: latest\n    dir: .\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "tag with {version}"},
		{"go tag with a module", "  a/b:\n    source: s\n    kind: go\n    module: m\n    repository: o/r\n    tag: v{version}\n    dir: .\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "not named"},
		{"go dir escaping", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    dir: ../x\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"go dir windows", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    dir: 'C:\\x'\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"go dir unclean", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    dir: x//y\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"go dir dotted", "  a/b:\n    source: s\n    kind: go\n    repository: o/r\n    tag: v{version}\n    dir: a..b\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"node dir", "  a/b:\n    source: s\n    kind: node\n    package: p\n    dir: x\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"go without a module", "  a/b:\n    source: s\n    kind: go\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "module required"},
		{"go with options", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n    options:\n      linux:\n        build: [--x]\n", "v1.0.0\n", "a field of another kind"},
		{"node ok", "  a/b:\n    source: s\n    kind: node\n    package: \"@o/p\"\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"unknown key", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n    colour: red\n", "v1.0.0\n", "colour"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, head+tc.yaml, map[string]string{"a/b": tc.versions})
			if tc.name == "bad name" {
				write(t, dir, head+tc.yaml, map[string]string{"ab": tc.versions})
			}
			_, err := Load(dir)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// A versions file's comments and blank lines are read past; the
// bump's append lands one tag per line.
func TestVersionsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "versions")
	if err := os.WriteFile(p, []byte("# first\nv1.0.0\n\n  v1.1.0 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AppendVersions(p, []string{"v1.2.0", "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	vs, err := ReadVersions(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(vs, " ") != "v1.0.0 v1.1.0 v1.2.0 v2.0.0" {
		t.Errorf("read %v", vs)
	}
}

// Expand fills the version's number, the tag and the platform's
// executable suffix; PlatformsOf orders the platforms as pb spells
// them whatever the recipe's order.
func TestExpandAndPlatforms(t *testing.T) {
	if got := Expand("x-{version}-{tag}{exe}", "v36.2", "windows/amd64"); got != "x-36.2-v36.2.exe" {
		t.Errorf("windows: %s", got)
	}
	if got := Expand("bin/e{exe}", "v1.2.3", "linux/arm64"); got != "bin/e" {
		t.Errorf("linux: %s", got)
	}
	m := &Plugin{Member: "bin/e", Members: map[string]string{"darwin/amd64": "x64/e"}}
	if m.MemberOf("darwin/amd64") != "x64/e" || m.MemberOf("linux/amd64") != "bin/e" {
		t.Errorf("members: %s %s", m.MemberOf("darwin/amd64"), m.MemberOf("linux/amd64"))
	}
	p := &Plugin{Platforms: []string{"windows/arm64", "linux/amd64"}, Assets: map[string]string{"darwin/arm64": "a"}}
	if got := strings.Join(p.PlatformsOf(), " "); got != "linux/amd64 darwin/arm64 windows/arm64" {
		t.Errorf("platforms %s", got)
	}
	for v, ok := range map[string]bool{"v1.2.3": true, "v36.2": true, "v1": false, "1.2.3": false, "v01.2": false, "v1.2.3-rc1": false, "v1.2.3+b": false} {
		if IsVersion(v) != ok {
			t.Errorf("IsVersion(%q) = %v", v, !ok)
		}
	}
}
