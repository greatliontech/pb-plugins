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

var head = "registry: r.example/p\nbase: r.example/base@sha256:" + "ab" + "\ntoolchains:\n  rust: 1.98.1\n  swift: 6.4.0\n  dart: 3.13.5\n  jvm: 21.0.12.1+1\n  node: 24.21.0\nsdks:\n  swift: " + strings.Repeat("ab", 32) + "\nplugins:\n"

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
		{"unknown kind", "  a/b:\n    source: s\n    kind: zig\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "unknown kind"},
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
		{"swift ok", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64, darwin/arm64]\n", "v1.0.0\n", ""},
		{"swift without product", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "product"},
		{"swift tag without version", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: v1\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "tag with {version}"},
		{"swift product path", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: bin/e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a bare name"},
		{"swift on windows", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64, windows/amd64]\n", "v1.0.0\n", "no runner path builds the kind for windows/amd64"},
		{"bazel on windows/arm64", "  a/b:\n    source: s\n    kind: bazel\n    repository: o/r\n    tag: \"{version}\"\n    archive: https://x/{version}.tar.gz\n    target: //t\n    output: o\n    entrypoint: e\n    platforms: [windows/arm64]\n", "v1.0.0\n", "no runner path builds the kind for windows/arm64"},
		{"a line", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n    line: v1\n", "v1.27.6\n", ""},
		{"a line with a leading zero", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n    line: v01\n", "v1.27.6\n", "no major version"},
		{"a line that is no major", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n    line: 1.x\n", "v1.27.6\n", "no major version"},
		{"a version outside the line", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n    line: v1\n", "v1.27.6\nv2.0.0\n", "outside the line v1"},
		{"node runtime ok", "  a/b:\n    source: s\n    kind: node\n    package: \"@o/p\"\n    runtime: true\n    script: bin/e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"node runtime without script", "  a/b:\n    source: s\n    kind: node\n    package: p\n    runtime: true\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "runtime and script go together"},
		{"node script without runtime", "  a/b:\n    source: s\n    kind: node\n    package: p\n    script: e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "runtime and script go together"},
		{"node script escaping", "  a/b:\n    source: s\n    kind: node\n    package: p\n    runtime: true\n    script: ../e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative path"},
		{"jvm ok", "  a/b:\n    source: s\n    kind: jvm\n    maven: g.h:a\n    classifier: c\n    extension: sh\n    checksum: sha1\n    modules: [java.base, jdk.unsupported]\n    entrypoint: e\n    platforms: [linux/amd64, windows/arm64]\n", "v1.0.0\n", ""},
		{"jvm without maven", "  a/b:\n    source: s\n    kind: jvm\n    checksum: sha256\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "maven group:artifact required"},
		{"jvm without checksum", "  a/b:\n    source: s\n    kind: jvm\n    maven: g:a\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "checksum sha256 or sha1 required"},
		{"jvm checksum of another kind", "  a/b:\n    source: s\n    kind: jvm\n    maven: g:a\n    checksum: md5\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "checksum sha256 or sha1 required"},
		{"jvm module name", "  a/b:\n    source: s\n    kind: jvm\n    maven: g:a\n    checksum: sha1\n    modules: [\"java base\"]\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no module name"},
		{"jvm classifier path", "  a/b:\n    source: s\n    kind: jvm\n    maven: g:a\n    classifier: a/b\n    checksum: sha1\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare name"},
		{"jvm foreign field", "  a/b:\n    source: s\n    kind: jvm\n    maven: g:a\n    checksum: sha1\n    crate: c\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"dart ok", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: p-v{version}\n    dir: packages/p\n    main: bin/m.dart\n    entrypoint: e\n    platforms: [linux/amd64, windows/arm64]\n", "v1.0.0\n", ""},
		{"dart at the root", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: bin/m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"dart without dir", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    main: bin/m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"dart dir escaping", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: ../p\n    main: bin/m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"dart main not a dart file", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: bin/m\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative Dart file"},
		{"dart dir with a drive", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: C:/p\n    main: bin/m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative directory"},
		{"dart main with a drive", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: C:m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative Dart file"},
		{"dart main absolute", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: /m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no relative Dart file"},
		{"dart foreign field", "  a/b:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: m.dart\n    product: p\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"swift foreign field", "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    crate: c\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"rust ok", "  a/b:\n    source: s\n    kind: rust\n    crate: c-d\n    bin: e2\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", ""},
		{"rust crate as a flag", "  a/b:\n    source: s\n    kind: rust\n    crate: --offline\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a letter first"},
		{"rust without crate", "  a/b:\n    source: s\n    kind: rust\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "crate required"},
		{"rust bin path", "  a/b:\n    source: s\n    kind: rust\n    crate: c\n    bin: bin/e\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "no bare name"},
		{"rust foreign field", "  a/b:\n    source: s\n    kind: rust\n    crate: c\n    module: m\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
		{"go with crate", "  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    crate: c\n    entrypoint: e\n    platforms: [linux/amd64]\n", "v1.0.0\n", "a field of another kind"},
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

// The bazel, rust, swift and dart kinds build on the platform itself; the
// others cross-build from one host.
func TestNative(t *testing.T) {
	for k, want := range map[Kind]bool{KindGo: false, KindNode: false, KindRelease: false, KindBazel: true, KindRust: true, KindSwift: true, KindDart: true, KindJvm: false} {
		if got := k.Native(); got != want {
			t.Errorf("%s native: %v, want %v", k, got, want)
		}
	}
}

// The image's process per kind: one executable at the root, the jvm
// kind's java over the jar named relative to the root, processes
// forked rather than spawned through the helper; the jar named by
// the artifact, version, classifier and extension; the modules the
// kind's unless the recipe names its own.
func TestArgvAndJar(t *testing.T) {
	p := &Plugin{Kind: KindGo, Entrypoint: "protoc-gen-x"}
	if got := strings.Join(p.Argv(), " "); got != "/protoc-gen-x" {
		t.Errorf("a go recipe's process: %q", got)
	}
	n := &Plugin{Kind: KindNode, Entrypoint: "protoc-gen-ts_proto", Package: "ts-proto", Runtime: true, Script: "protoc-gen-ts_proto"}
	if got := strings.Join(n.Argv(), " "); got != "/node app/node_modules/ts-proto/protoc-gen-ts_proto" {
		t.Errorf("a runtime node recipe's process: %q", got)
	}
	if got := strings.Join((&Plugin{Kind: KindNode, Entrypoint: "protoc-gen-x", Package: "x"}).Argv(), " "); got != "/protoc-gen-x" {
		t.Errorf("a compiled node recipe's process: %q", got)
	}
	j := &Plugin{Kind: KindJvm, Entrypoint: "protoc-gen-scala", Maven: "com.thesamet.scalapb:protoc-gen-scala", Classifier: "unix", Extension: "sh"}
	if got := strings.Join(j.Argv(), " "); got != "/jre/bin/java -Djdk.lang.Process.launchMechanism=FORK -jar protoc-gen-scala.jar" {
		t.Errorf("a jvm recipe's process: %q", got)
	}
	if got := j.JarFile("v0.11.20"); got != "protoc-gen-scala-0.11.20-unix.sh" {
		t.Errorf("the jar's name: %q", got)
	}
	k := &Plugin{Kind: KindJvm, Entrypoint: "protoc-gen-grpc-kotlin", Maven: "io.grpc:protoc-gen-grpc-kotlin", Classifier: "jdk8"}
	if got := k.JarFile("v1.5.0"); got != "protoc-gen-grpc-kotlin-1.5.0-jdk8.jar" {
		t.Errorf("the jar's name with a classifier: %q", got)
	}
	if got := (&Plugin{Kind: KindJvm, Maven: "g:a"}).JarFile("v2.0.0"); got != "a-2.0.0.jar" {
		t.Errorf("the plain jar's name: %q", got)
	}
	if got := strings.Join(k.JvmModules(), ","); got != strings.Join(JvmModules, ",") {
		t.Errorf("the kind's modules: %q", got)
	}
	if got := strings.Join((&Plugin{Kind: KindJvm, Modules: []string{"java.base"}}).JvmModules(), ","); got != "java.base" {
		t.Errorf("the recipe's modules: %q", got)
	}
}

// The catalog's kinds are known; another name is not.
func TestKnown(t *testing.T) {
	for _, k := range []Kind{KindGo, KindNode, KindRelease, KindBazel, KindRust, KindSwift, KindDart, KindJvm} {
		if !k.Known() {
			t.Errorf("%s unknown", k)
		}
	}
	if Kind("rustt").Known() || Kind("").Known() {
		t.Error("a name the catalog does not define is known")
	}
}

// A recipe of a kind the pipeline pins a toolchain for needs the
// catalog's pin; a pin for a kind the pipeline does not install is
// refused.
func TestToolchainsPinned(t *testing.T) {
	swift := "  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n"
	sdk := "sdks:\n  swift: " + strings.Repeat("ab", 32) + "\n"
	dart := "  c/d:\n    source: s\n    kind: dart\n    repository: o/r\n    tag: v{version}\n    dir: .\n    main: m.dart\n    entrypoint: e\n    platforms: [linux/amd64]\n"
	dir := t.TempDir()
	write(t, dir, "registry: r.example/p\nbase: r.example/base@sha256:ab\ntoolchains:\n  swift: 6.4.0\n"+sdk+"plugins:\n"+swift+dart, map[string]string{"a/b": "v1.0.0\n", "c/d": "v1.0.0\n"})
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "a dart recipe needs the dart toolchain pinned") {
		t.Errorf("a dart recipe without the pin: %v", err)
	}
	node := "  e/f:\n    source: s\n    kind: node\n    package: p\n    runtime: true\n    script: e\n    entrypoint: e\n    platforms: [linux/amd64]\n"
	write(t, dir, "registry: r.example/p\nbase: r.example/base@sha256:ab\ntoolchains:\n  swift: 6.4.0\n"+sdk+"plugins:\n"+swift+node, map[string]string{"a/b": "v1.0.0\n", "e/f": "v1.0.0\n"})
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "a node recipe running under node's runtime needs the node toolchain pinned") {
		t.Errorf("a runtime node recipe without the pin: %v", err)
	}
	compiled := "  e/f:\n    source: s\n    kind: node\n    package: p\n    entrypoint: e\n    platforms: [linux/amd64]\n"
	write(t, dir, "registry: r.example/p\nbase: r.example/base@sha256:ab\ntoolchains:\n  swift: 6.4.0\n"+sdk+"plugins:\n"+swift+compiled, map[string]string{"a/b": "v1.0.0\n", "e/f": "v1.0.0\n"})
	if _, err := Load(dir); err != nil {
		t.Errorf("a compiled node recipe needs no pin: %v", err)
	}
	for _, tc := range []struct{ name, toolchains, want string }{
		{"swift unpinned", "toolchains:\n  rust: 1.98.1\n" + sdk, "a swift recipe needs the swift toolchain pinned"},
		{"swift pinned", "toolchains:\n  swift: 6.4.0\n" + sdk, ""},
		{"a floating pin", "toolchains:\n  swift: \"6.3\"\n" + sdk, "is no release number"},
		{"a jvm pin without its build number", "toolchains:\n  swift: 6.4.0\n  jvm: 21.0.12.1\n" + sdk, "is no release number"},
		{"a jvm pin as Temurin names it", "toolchains:\n  swift: 6.4.0\n  jvm: 21.0.12.1+1\n" + sdk, ""},
		{"a kind the pipeline does not pin", "toolchains:\n  swift: 6.4.0\n  go: 1.25.0\n" + sdk, "names no kind the pipeline pins"},
		{"swift's SDK unpinned", "toolchains:\n  swift: 6.4.0\n", "a swift recipe needs the swift SDK's checksum pinned"},
		{"an SDK pin that is no sha256", "toolchains:\n  swift: 6.4.0\nsdks:\n  swift: 47d2\n", "is no sha256"},
		{"an SDK for a kind the pipeline installs none for", "toolchains:\n  swift: 6.4.0\n" + sdk + "  rust: " + strings.Repeat("ab", 32) + "\n", "names no kind the pipeline installs an SDK for"},
	} {
		dir := t.TempDir()
		write(t, dir, "registry: r.example/p\nbase: r.example/base@sha256:ab\n"+tc.toolchains+"plugins:\n"+swift, map[string]string{"a/b": "v1.0.0\n"})
		_, err := Load(dir)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
	}
}

// The go, rust and swift kinds' Linux executables are static and take no
// base; the dart kind's links the C library and takes it, as every
// other kind's may.
func TestNeedsBase(t *testing.T) {
	for k, want := range map[Kind]bool{KindGo: false, KindRust: false, KindSwift: false, KindNode: true, KindRelease: true, KindBazel: true, KindDart: true, KindJvm: true} {
		if got := k.NeedsBase(); got != want {
			t.Errorf("%s needs a base: %v, want %v", k, got, want)
		}
	}
}
