package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"github.com/greatliontech/pb-plugins/internal/github"
)

func load(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The plan lists what the registry lacks: a native kind one tree job
// per platform on that platform's runner, a cross kind one job for
// every platform, one build per version; a published version is
// left out, every version under --all, and a registry that cannot
// answer fails the plan.
func TestPlan(t *testing.T) {
	c := load(t)
	published := map[string]bool{c.Reference("protocolbuffers/go", "v1.36.12"): true}
	exists := func(_ context.Context, ref string) (bool, error) { return published[ref], nil }
	plan, err := Compute(context.Background(), c, exists, false)
	if err != nil {
		t.Fatal(err)
	}
	// A rust or swift tree carries the catalog's pinned toolchain; a
	// bazel one none.
	seen := map[string]bool{}
	for _, tr := range plan.Trees.Include {
		seen[tr.Kind] = true
		switch pin := c.Toolchains[tr.Kind]; {
		case (tr.Kind == "rust" || tr.Kind == "swift") && pin == "":
			t.Fatalf("the fixture pins no %s toolchain", tr.Kind)
		case tr.Toolchain != pin:
			t.Errorf("%s: toolchain %q, want the catalog's %q", tr.Tree, tr.Toolchain, pin)
		}
	}
	if !seen["rust"] || !seen["swift"] || !seen["bazel"] {
		t.Errorf("the plan's tree kinds: %v", seen)
	}
	builds := map[string]bool{}
	for _, b := range plan.Builds.Include {
		builds[b.Build] = true
	}
	if builds["protocolbuffers-go-v1.36.12"] || !builds["grpc-web-v2.1.1"] || builds["bufbuild-es-v2.15.0"] {
		t.Errorf("builds %v", builds)
	}
	var web, es []Tree
	for _, tr := range plan.Trees.Include {
		switch tr.Build {
		case "grpc-web-v2.1.1":
			web = append(web, tr)
		case "bufbuild-es-v2.15.0":
			t.Errorf("a cross kind among the native trees: %+v", tr)
		}
	}
	for _, tr := range plan.Cross.Include {
		switch tr.Build {
		case "bufbuild-es-v2.15.0":
			es = append(es, tr)
		case "protocolbuffers-go-v1.36.12", "grpc-web-v2.1.1":
			t.Errorf("wrong cross job %+v", tr)
		}
	}
	if len(web) != 5 || web[0].Platforms != "linux/amd64" || web[0].Runner != "ubuntu-24.04" || web[3].Runner != "macos-15" || web[4].Tree != "grpc-web-v2.1.1-windows-amd64" {
		t.Errorf("web trees %+v", web)
	}
	if len(es) != 1 || es[0].Runner != CrossRunner || es[0].Platforms != strings.Join(catalog.Platforms, ",") || es[0].Tree != "bufbuild-es-v2.15.0-cross" {
		t.Errorf("es trees %+v", es)
	}
	if !plan.Any {
		t.Error("Any false")
	}
	all, err := Compute(context.Background(), c, exists, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Cross.Include) != len(plan.Cross.Include)+1 || len(all.Builds.Include) != len(plan.Builds.Include) {
		t.Errorf("--all: %d cross, %d builds; filtered %d, %d", len(all.Cross.Include), len(all.Builds.Include), len(plan.Cross.Include), len(plan.Builds.Include))
	}
	every := func(context.Context, string) (bool, error) { return true, nil }
	none, err := Compute(context.Background(), c, every, false)
	if err != nil || none.Any || len(none.Trees.Include) != 0 || len(none.Cross.Include) != 0 || !strings.Contains(none.String(), "nothing to build") {
		t.Errorf("all published: %v %+v", err, none)
	}
	broken := func(context.Context, string) (bool, error) { return false, errors.New("registry down") }
	if _, err := Compute(context.Background(), c, broken, false); err == nil {
		t.Error("a registry that cannot answer planned")
	}
}

// The report's first line carries the digest, published or
// unchanged; anything else is refused.
func TestParseReport(t *testing.T) {
	for in, want := range map[string]string{
		"r.example/p/a/b:v1@sha256:ab published\n  linux/amd64 sha256:cd\n": "sha256:ab",
		"r.example/p/a/b:v1@sha256:ab unchanged\n":                          "sha256:ab",
		"r.example/p/a/b:v1@sha256:ab":                                      "",
		"error: nope\n":                                                     "",
		"r.example/p/a/b:v1 published\n":                                    "",
	} {
		got, err := ParseReport(in)
		if (err == nil) != (want != "") || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
}

// fakeRegistry holds the tags that exist (signature tags among
// them) and the digest each published tag names.
type fakeRegistry struct {
	tags    map[string]bool
	digests map[string]string
}

func (f fakeRegistry) PlatformImage(_ context.Context, ref, platform string) (string, error) {
	repo, _, _ := strings.Cut(ref, "@")
	return repo + "@sha256:" + strings.ReplaceAll(platform, "/", "") + "0000", nil
}

func (f fakeRegistry) TagExists(_ context.Context, ref string) (bool, error) { return f.tags[ref], nil }

func (f fakeRegistry) Digest(_ context.Context, ref string) (string, error) {
	d, ok := f.digests[ref]
	if !ok {
		return "", errors.New("no such tag")
	}
	return d, nil
}

// fakeTool writes a shell script that records its arguments and
// prints the report given.
// logOf is a fake tool's log as the shell wrote it, windows' line
// endings read as LF.
func logOf(path string) []byte {
	b, _ := os.ReadFile(path)
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
}

func fakeTool(t *testing.T, dir, name, report string) (path, log string) {
	t.Helper()
	log = filepath.Join(dir, name+".log")
	path = filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nprintf '" + report + "'\n"
	if runtime.GOOS == "windows" {
		// A batch file: the arguments as one line to the log, the
		// report (one line, its own line break) to standard output.
		path += ".cmd"
		script = "@echo off\r\necho %*>>\"" + log + "\"\r\necho " + strings.TrimSuffix(report, "\\n") + "\r\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, log
}

// Publish composes `pb plugin build` from the catalog: the reference,
// the entrypoint, every platform's tree, and the base's platform
// image for a Linux tree of a kind needing one; then signs the
// list's digest recursively where no signature tag exists, and
// leaves a signed one alone. A missing tree refuses before anything
// runs.
func TestPublish(t *testing.T) {
	c := load(t)
	dir := t.TempDir()
	trees := filepath.Join(dir, "trees")
	for _, pl := range c.Plugins["protocolbuffers/js"].PlatformsOf() {
		goos, arch, _ := strings.Cut(pl, "/")
		d := filepath.Join(trees, goos+"-"+arch)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "protoc-gen-js"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ref := c.Reference("protocolbuffers/js", "v4.0.3")
	pb, pbLog := fakeTool(t, dir, "pb", ref+"@sha256:1111 published\\n")
	cosign, cosignLog := fakeTool(t, dir, "cosign", "")
	var out strings.Builder
	reg := fakeRegistry{tags: map[string]bool{}, digests: map[string]string{}}
	p := &Publisher{Catalog: c, Registry: reg, PB: pb, Cosign: cosign, Trees: trees, Out: &out}
	if err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3"); err != nil {
		t.Fatal(err)
	}
	args := logOf(pbLog)
	want := []string{
		"plugin build " + ref + " --entrypoint /protoc-gen-js",
		"--platform linux/amd64=" + filepath.Join(trees, "linux-amd64") + " --base linux/amd64=gcr.io/distroless/cc-debian13@sha256:linuxamd640000",
		"--platform linux/arm64=" + filepath.Join(trees, "linux-arm64") + " --base linux/arm64=gcr.io/distroless/cc-debian13@sha256:linuxarm640000",
		"--platform darwin/amd64=" + filepath.Join(trees, "darwin-amd64") + " --platform darwin/arm64=",
		"--platform windows/amd64=" + filepath.Join(trees, "windows-amd64") + "\n",
	}
	for _, w := range want {
		if !strings.Contains(string(args), w) {
			t.Errorf("pb args %q lack %q", args, w)
		}
	}
	if strings.Contains(string(args), "--base darwin") || strings.Contains(string(args), "--base windows") {
		t.Errorf("a base for a non-Linux platform: %q", args)
	}
	sig := logOf(cosignLog)
	repo, _, _ := strings.Cut(ref, ":v")
	if string(sig) != "sign --yes --recursive "+repo+"@sha256:1111\n" {
		t.Errorf("cosign args %q", sig)
	}
	if !strings.Contains(out.String(), "published") || !strings.Contains(out.String(), "@sha256:1111 signed\n") {
		t.Errorf("report %q", out.String())
	}

	// Signed already: cosign is not run.
	os.Remove(cosignLog)
	reg.tags[repo+":sha256-1111.sig"] = true
	out.Reset()
	if err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cosignLog); err == nil {
		t.Error("cosign ran over a signed digest")
	}
	if !strings.Contains(out.String(), "signed already") {
		t.Errorf("report %q", out.String())
	}

	// A tree missing refuses before pb runs.
	os.Remove(pbLog)
	if err := os.Remove(filepath.Join(trees, "windows-amd64", "protoc-gen-js")); err != nil {
		t.Fatal(err)
	}
	err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3")
	if err == nil || !strings.Contains(err.Error(), "no tree for windows/amd64") {
		t.Errorf("missing tree: %v", err)
	}
	if _, err := os.Stat(pbLog); err == nil {
		t.Error("pb ran without every tree")
	}

	// A tag published already is not built again: its digest is read
	// and signed where unsigned, and left alone where signed.
	os.Remove(pbLog)
	os.Remove(cosignLog)
	reg.tags[ref] = true
	reg.digests[ref] = "sha256:3333"
	out.Reset()
	if err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pbLog); err == nil {
		t.Error("pb ran over a published tag")
	}
	sig = logOf(cosignLog)
	if string(sig) != "sign --yes --recursive "+repo+"@sha256:3333\n" || !strings.Contains(out.String(), "@sha256:3333 published already") {
		t.Errorf("published tag: cosign %q, report %q", sig, out.String())
	}
	reg.tags[repo+":sha256-3333.sig"] = true
	os.Remove(cosignLog)
	if err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cosignLog); err == nil {
		t.Error("cosign ran over a published, signed tag")
	}
	delete(reg.tags, ref)

	// The go kind takes no base.
	for _, pl := range c.Plugins["protocolbuffers/go"].PlatformsOf() {
		goos, arch, _ := strings.Cut(pl, "/")
		d := filepath.Join(trees, goos+"-"+arch)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "protoc-gen-go"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	goRef := c.Reference("protocolbuffers/go", "v1.36.12")
	p.PB, pbLog = fakeTool(t, dir, "pb2", goRef+"@sha256:2222 unchanged\\n")
	if err := p.Publish(context.Background(), "protocolbuffers/go", "v1.36.12"); err != nil {
		t.Fatal(err)
	}
	args = logOf(pbLog)
	if strings.Contains(string(args), "--base") {
		t.Errorf("a base for the go kind: %q", args)
	}
}

// The bump appends the stable versions above each plugin's highest,
// ascending and deduplicated, never a prerelease, never a version
// below the highest, and reports them; an upstream that cannot be
// listed fails the bump.
func TestBump(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plugins", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// One go plugin under the real catalog's header.
	header := string(raw[:strings.Index(string(raw), "plugins:")])
	if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(header+"plugins:\n  a/b:\n    source: s\n    kind: go\n    module: m\n    package: .\n    entrypoint: e\n    platforms: [linux/amd64]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugins", "a", "b", "versions"), []byte("v1.0.0\nv1.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := []string{"v1.1.0", "v0.9.0", "v2.0.0", "v1.2.0-rc1", "v1.2.0", "v2.0.0", "v1.10.0", "junk"}
	added, err := Bump(context.Background(), c, func(context.Context, *catalog.Plugin) ([]string, error) { return found, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(added["a/b"], " "); got != "v1.2.0 v1.10.0 v2.0.0" {
		t.Errorf("added %q", got)
	}
	vs, _ := catalog.ReadVersions(c.VersionsPath("a/b"))
	if got := strings.Join(vs, " "); got != "v1.0.0 v1.1.0 v1.2.0 v1.10.0 v2.0.0" {
		t.Errorf("file %q", got)
	}
	if _, err := catalog.Load(dir); err != nil {
		t.Errorf("the bumped catalog does not load: %v", err)
	}
	again, err := Bump(context.Background(), c, func(context.Context, *catalog.Plugin) ([]string, error) { return found, nil })
	if err != nil || len(again) != 0 {
		t.Errorf("a second bump added %v, %v", again, err)
	}
	if _, err := Bump(context.Background(), c, func(context.Context, *catalog.Plugin) ([]string, error) { return nil, errors.New("down") }); err == nil {
		t.Error("an upstream down bumped")
	}
	// A frozen plugin is passed over: nothing appended, upstream not
	// even asked.
	c.Plugins["a/b"].Frozen = true
	asked := false
	frozen, err := Bump(context.Background(), c, func(context.Context, *catalog.Plugin) ([]string, error) { asked = true; return []string{"v9.0.0"}, nil })
	if err != nil || len(frozen) != 0 || asked {
		t.Errorf("a frozen plugin bumped: %v %v asked=%v", frozen, err, asked)
	}
}

// A plugin naming a line takes the line's versions from the bump
// alone: another major upstream releases beside it is passed over.
func TestBumpLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plugins", "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	header := string(raw[:strings.Index(string(raw), "plugins:")])
	if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(header+"plugins:\n  a/b:\n    source: s\n    kind: swift\n    repository: o/r\n    tag: \"{version}\"\n    product: e\n    entrypoint: e\n    platforms: [linux/amd64]\n    line: v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugins", "a", "b", "versions"), []byte("v1.27.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := []string{"v2.2.3", "v1.27.7", "v2.0.0", "v1.28.0"}
	added, err := Bump(context.Background(), c, func(context.Context, *catalog.Plugin) ([]string, error) { return found, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(added["a/b"], " "); got != "v1.27.7 v1.28.0" {
		t.Errorf("added %q", got)
	}
	if _, err := catalog.Load(dir); err != nil {
		t.Errorf("the bumped catalog does not load: %v", err)
	}
}

// A tag reads as a version through its template's prefix and suffix,
// two-component versions included; another shape is none.
func TestTagVersion(t *testing.T) {
	cases := []struct{ tag, prefix, suffix, want string }{
		{"v36.2", "v", "", "v36.2"},
		{"2.1.1", "", "", "v2.1.1"},
		{"v1.84.0", "v", "", "v1.84.0"},
		{"v1.84.0-pre1", "v", "", ""},
		{"cmd/protoc-gen-go-grpc/v1.6.2", "cmd/protoc-gen-go-grpc/v", "", "v1.6.2"},
		{"v1.6.2", "cmd/protoc-gen-go-grpc/v", "", ""},
		{"release-1.2.3", "v", "", ""},
		{"v", "v", "", ""},
	}
	for _, tc := range cases {
		got, ok := TagVersion(tc.tag, tc.prefix, tc.suffix)
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("%s: %q %v", tc.tag, got, ok)
		}
	}
}

// A go recipe built from a repository tag discovers its versions
// through the repository's releases, read through the tag template;
// one without a repository through the module proxy's list.
func TestUpstreamGoFromATag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"tag_name":"1.4.1"},{"tag_name":"v0.4"},{"tag_name":"1.4.0","prerelease":true}]`)
	}))
	defer srv.Close()
	saved := github.API
	github.API = srv.URL
	defer func() { github.API = saved }()
	p := &catalog.Plugin{Kind: catalog.KindGo, Dir: ".", Repository: "o/r", Tag: "{version}"}
	vs, err := Upstream(context.Background(), p)
	if err != nil || strings.Join(vs, " ") != "v1.4.1" {
		t.Fatalf("versions from the releases: %v %v", vs, err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/m.example/x/@v/list" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "v1.0.0\nv1.1.0\n")
	}))
	defer proxy.Close()
	savedProxy := endpoints.Proxy
	endpoints.Proxy = proxy.URL
	defer func() { endpoints.Proxy = savedProxy }()
	vs, err = Upstream(context.Background(), &catalog.Plugin{Kind: catalog.KindGo, Module: "m.example/x"})
	if err != nil || strings.Join(vs, " ") != "v1.0.0 v1.1.0" {
		t.Fatalf("versions from the proxy: %v %v", vs, err)
	}
}

// A release recipe naming a Maven artifact discovers its versions
// from Maven Central's metadata; one naming an npm package from the
// registry; one naming neither from the repository's releases.
func TestUpstreamReleaseSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/io/grpc/protoc-gen-grpc-java/maven-metadata.xml":
			fmt.Fprint(w, `<?xml version="1.0"?><metadata><groupId>io.grpc</groupId><artifactId>protoc-gen-grpc-java</artifactId><versioning><latest>1.84.0</latest><versions><version>1.83.1</version><version>1.84.0</version></versions></versioning></metadata>`)
		case "/grpc-tools":
			fmt.Fprint(w, `{"versions":{"1.13.0":{},"1.13.1":{}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	savedMaven, savedNpm := endpoints.Maven, endpoints.Npm
	endpoints.Maven, endpoints.Npm = srv.URL, srv.URL
	defer func() { endpoints.Maven, endpoints.Npm = savedMaven, savedNpm }()
	vs, err := Upstream(context.Background(), &catalog.Plugin{Kind: catalog.KindRelease, Maven: "io.grpc:protoc-gen-grpc-java"})
	if err != nil || strings.Join(vs, " ") != "v1.83.1 v1.84.0" {
		t.Fatalf("maven: %v %v", vs, err)
	}
	vs, err = Upstream(context.Background(), &catalog.Plugin{Kind: catalog.KindRelease, Npm: "grpc-tools"})
	sort.Strings(vs)
	if err != nil || strings.Join(vs, " ") != "v1.13.0 v1.13.1" {
		t.Fatalf("npm: %v %v", vs, err)
	}
}

// A swift recipe discovers its versions through the repository's
// releases, as a bazel one does; a tag of another shape is passed.
func TestUpstreamSwift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"tag_name":"1.38.1"},{"tag_name":"protoc-artifactbundle-v32.1"},{"tag_name":"1.38.0","prerelease":true}]`)
	}))
	defer srv.Close()
	saved := github.API
	github.API = srv.URL
	defer func() { github.API = saved }()
	vs, err := Upstream(context.Background(), &catalog.Plugin{Kind: catalog.KindSwift, Repository: "o/r", Tag: "{version}"})
	if err != nil || strings.Join(vs, " ") != "v1.38.1" {
		t.Fatalf("versions from the releases: %v %v", vs, err)
	}
}

// A rust recipe discovers its versions from crates.io, the yanked
// ones left out.
func TestUpstreamRust(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/crates/c/versions" || !strings.Contains(r.Header.Get("User-Agent"), "pb-plugins") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"versions":[{"num":"0.9.0","yanked":false},{"num":"0.8.1","yanked":true},{"num":"0.8.0","yanked":false}]}`)
	}))
	defer srv.Close()
	saved := endpoints.Crates
	endpoints.Crates = srv.URL
	defer func() { endpoints.Crates = saved }()
	vs, err := Upstream(context.Background(), &catalog.Plugin{Kind: catalog.KindRust, Crate: "c"})
	if err != nil || strings.Join(vs, " ") != "v0.9.0 v0.8.0" {
		t.Fatalf("crates: %v %v", vs, err)
	}
}
