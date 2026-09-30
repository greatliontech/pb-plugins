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
		{"foreign field", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    target: t\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"platform unknown", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/386]\n", "v1.0.0\n", "is none of"},
		{"platform twice", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64, linux/amd64]\n", "v1.0.0\n", "twice"},
		{"no platforms", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n", "v1.0.0\n", "no platforms"},
		{"entrypoint path", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: bin/e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare name"},
		{"version prerelease", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0-rc1\n", "is no vMAJOR"},
		{"version order", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.1.0\nv1.0.0\n", "not ascending"},
		{"version twice", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\nv1.0.0\n", "not ascending"},
		{"release ok", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    member: bin/e{exe}\n    entrypoint: e\n    assets:\n      linux/amd64: x.tar.gz\n      windows/amd64: x.zip\n", "v1.0.0\n", ""},
		{"release with platforms", "  a/b:\n    source: s\n    kind: release\n    repository: o/r\n    tag: v{version}\n    member: bin/e\n    entrypoint: e\n    platforms: [linux/amd64]\n    assets:\n      linux/amd64: x.tar.gz\n", "v1.0.0\n", "a field of another kind"},
		{"bazel ok", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    strip: 1\n    files: plugins\n    target: //t\n    output: bazel-bin/t{exe}\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"bazel files path", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    files: ../plugins\n    target: //t\n    output: bazel-bin/t\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare directory"},
		{"bazel options os", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: v{version}\n    archive: https://x/{version}.tar.gz\n    target: //t\n    output: bazel-bin/t\n    entrypoint: e\n    platforms: [linux/amd64]\n    options:\n      freebsd:\n        build: [--x]\n", "v1.0.0\n", "no operating system"},
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
