package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greatliontech/pb-plugins/internal/catalog"
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

type fakeRegistry struct {
	sigs map[string]bool
}

func (f fakeRegistry) PlatformImage(_ context.Context, ref, platform string) (string, error) {
	repo, _, _ := strings.Cut(ref, "@")
	return repo + "@sha256:" + strings.ReplaceAll(platform, "/", "") + "0000", nil
}

func (f fakeRegistry) TagExists(_ context.Context, ref string) (bool, error) { return f.sigs[ref], nil }

// fakeTool writes a shell script that records its arguments and
// prints the report given.
func fakeTool(t *testing.T, dir, name, report string) (path, log string) {
	t.Helper()
	log = filepath.Join(dir, name+".log")
	path = filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nprintf '" + report + "'\n"
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
	reg := fakeRegistry{sigs: map[string]bool{}}
	p := &Publisher{Catalog: c, Registry: reg, PB: pb, Cosign: cosign, Trees: trees, Out: &out}
	if err := p.Publish(context.Background(), "protocolbuffers/js", "v4.0.3"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(pbLog)
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
	sig, _ := os.ReadFile(cosignLog)
	repo, _, _ := strings.Cut(ref, ":v")
	if string(sig) != "sign --yes --recursive --new-bundle-format=false "+repo+"@sha256:1111\n" {
		t.Errorf("cosign args %q", sig)
	}
	if !strings.Contains(out.String(), "published") || !strings.Contains(out.String(), "@sha256:1111 signed\n") {
		t.Errorf("report %q", out.String())
	}

	// Signed already: cosign is not run.
	os.Remove(cosignLog)
	reg.sigs[repo+":sha256-1111.sig"] = true
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
	args, _ = os.ReadFile(pbLog)
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
