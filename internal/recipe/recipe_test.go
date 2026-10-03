package recipe

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

func tarGz(t *testing.T, entries map[string]string, links map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		mode := int64(0o644)
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if strings.Contains(name, "bin/") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	for name, target := range links {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target}); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

// A release asset's member lands as the tree's entrypoint from a
// tar.gz and from a zip; a missing member and a status other than
// 200 refuse.
func TestExtractURL(t *testing.T) {
	files := map[string][]byte{
		"/a.tar.gz": tarGz(t, map[string]string{"bin/": "", "bin/e": "elf", "readme": "r"}, nil),
		"/a.zip":    zipped(t, map[string]string{"bin/e.exe": "pe", "readme": "r"}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "e")
	if err := extractURL(context.Background(), srv.URL+"/a.tar.gz", "bin/e", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "elf" {
		t.Errorf("tar member %q", b)
	}
	if err := extractURL(context.Background(), srv.URL+"/a.zip", "bin/e.exe", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "pe" {
		t.Errorf("zip member %q", b)
	}
	if err := extractURL(context.Background(), srv.URL+"/a.zip", "bin/other", out); err == nil || !strings.Contains(err.Error(), "no member") {
		t.Errorf("missing member: %v", err)
	}
	if err := extractURL(context.Background(), srv.URL+"/missing.zip", "bin/e", out); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing asset: %v", err)
	}
	if err := extractURL(context.Background(), srv.URL+"/a.tar.gz", "bin/e", filepath.Join(dir, "deep", "e")); err != nil {
		t.Errorf("a missing directory is created: %v", err)
	}
}

// A source archive extracts with its leading components stripped,
// executables keeping their bit, links within the archive kept and
// a link escaping it refused; the recipe's files copy in over it.
func TestExtractTarInto(t *testing.T) {
	archive := tarGz(t, map[string]string{"root-1.0/": "", "root-1.0/BUILD": "b", "root-1.0/src/": "", "root-1.0/src/bin/x": "x"}, map[string]string{"root-1.0/src/link": "bin/x"})
	escaping := tarGz(t, map[string]string{"root-1.0/a": "a"}, map[string]string{"root-1.0/link": "../../etc/passwd"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.tar.gz":
			w.Write(archive)
		case "/escape.tar.gz":
			w.Write(escaping)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	if err := extractTarInto(context.Background(), srv.URL+"/ok.tar.gz", 1, dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "BUILD")); string(b) != "b" {
		t.Errorf("BUILD %q", b)
	}
	// windows has no executable bit, and spells a link's target with
	// its own separator.
	if fi, err := os.Stat(filepath.Join(dir, "src", "bin", "x")); err != nil || (runtime.GOOS != "windows" && fi.Mode()&0o111 == 0) {
		t.Errorf("bin/x: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "src", "link")); err != nil || filepath.ToSlash(target) != "bin/x" {
		t.Errorf("link %q %v", target, err)
	}
	if err := extractTarInto(context.Background(), srv.URL+"/escape.tar.gz", 1, t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Errorf("escaping link: %v", err)
	}
	from := t.TempDir()
	os.MkdirAll(filepath.Join(from, "sub"), 0o755)
	os.WriteFile(filepath.Join(from, "BUILD.bazel"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(from, "sub", "x.cc"), []byte("cc"), 0o644)
	if err := copyTree(from, filepath.Join(dir, "plugins")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "plugins", "sub", "x.cc")); string(b) != "cc" {
		t.Errorf("copied %q", b)
	}
}

// A package's executable is read from its `bin`: the map's entry of
// the name, or the sole string; a `bin` naming neither refuses.
func TestBinScript(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"bin":{"protoc-gen-es":"bin/protoc-gen-es","other":"x"}}`), 0o644)
	manifest, err := readPackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := binScript(manifest, dir, "protoc-gen-es"); err != nil || got != filepath.Join(dir, "bin", "protoc-gen-es") {
		t.Errorf("map: %q %v", got, err)
	}
	if _, err := binScript(manifest, dir, "missing"); err == nil {
		t.Error("a name the map lacks resolved")
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"bin":"cli.js","type":"module"}`), 0o644)
	manifest, _ = readPackage(dir)
	if got, err := binScript(manifest, dir, "whatever"); err != nil || got != filepath.Join(dir, "cli.js") || manifest.Type != "module" {
		t.Errorf("string: %q %v %q", got, err, manifest.Type)
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o644)
	manifest, _ = readPackage(dir)
	if _, err := binScript(manifest, dir, "x"); err == nil {
		t.Error("no bin resolved")
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{`), 0o644)
	if _, err := readPackage(dir); err == nil {
		t.Error("a broken package.json read")
	}
}

// Build refuses a platform the plugin does not serve, and a native
// kind refuses a platform other than the host's, before anything
// is fetched or run.
func TestBuildRefusals(t *testing.T) {
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Build(context.Background(), c, "protocolbuffers/js", "v4.0.3", []string{"windows/arm64"}, out); err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Errorf("unserved platform: %v", err)
	}
	other := "linux/arm64"
	if Host() == other {
		other = "linux/amd64"
	}
	if err := Build(context.Background(), c, "grpc/web", "v2.1.1", []string{other}, out); err == nil || !strings.Contains(err.Error(), "builds on the platform itself") {
		t.Errorf("native kind off-host: %v", err)
	}
	if err := Build(context.Background(), c, "nobody/none", "v1.0.0", []string{"linux/amd64"}, out); err == nil {
		t.Error("an unknown plugin built")
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Errorf("a refusal wrote %v", entries)
	}
}

// The go kind cross-compiles the six platforms from one host
// (network: the module proxy). Runs where PBPLUGINS_LIVE is set.
func TestGoKindLive(t *testing.T) {
	if os.Getenv("PBPLUGINS_LIVE") == "" {
		t.Skip("PBPLUGINS_LIVE unset")
	}
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Build(context.Background(), c, "protocolbuffers/go", "v1.36.12", []string{"linux/amd64", "windows/arm64"}, out); err != nil {
		t.Fatal(err)
	}
	for _, pl := range []string{"linux/amd64", "windows/arm64"} {
		if fi, err := os.Stat(filepath.Join(TreeDir(out, pl), "protoc-gen-go")); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", pl, err)
		}
	}
}

// The go kind builds from a repository tag the module proxy does not
// list: an unprefixed tag, and a nested module at the root tag's
// commit with `{major}` in its path (network: GitHub and the module
// proxy). Runs where PBPLUGINS_LIVE is set.
func TestGoKindFromATagLive(t *testing.T) {
	if os.Getenv("PBPLUGINS_LIVE") == "" {
		t.Skip("PBPLUGINS_LIVE unset")
	}
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{"community/chrusty-jsonschema": "v1.4.1", "community/roadrunner-server-php-grpc": "v5.3.0"} {
		out := t.TempDir()
		if err := Build(context.Background(), c, name, version, []string{Host()}, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), c.Plugins[name].Entrypoint)); err != nil || fi.Size() == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The bazel command carries the operating system's startup options
// first, the short output root on windows, then the build with its
// options and the target.
func TestBazelArgs(t *testing.T) {
	p := &catalog.Plugin{Target: "//t", Options: map[string]catalog.BazelOptions{
		"windows": {Startup: []string{"--noworkspace_rc"}, Build: []string{"--config=windows"}},
	}}
	if got := strings.Join(bazelArgs(p, "windows"), " "); got != "--noworkspace_rc --output_user_root=C:/b/out --host_jvm_args=-Djava.net.preferIPv4Stack=true build -c opt --config=windows //t" {
		t.Errorf("windows: %s", got)
	}
	if got := strings.Join(bazelArgs(p, "linux"), " "); got != "--host_jvm_args=-Djava.net.preferIPv4Stack=true build -c opt //t" {
		t.Errorf("linux: %s", got)
	}
}

// bundleEntry hands bun a script whose extension it bundles: a bin
// with one is handed as is, an extensionless one through a copy
// beside it, `.cjs` by default and `.mjs` for a package of type
// module, so its relative requires still resolve.
func TestBundleEntry(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cli"), []byte("#!/usr/bin/env node\nrequire('./lib')\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "cli.js"), []byte("x"), 0o755)
	if got, err := bundleEntry(filepath.Join(dir, "cli.js"), "module"); err != nil || got != filepath.Join(dir, "cli.js") {
		t.Errorf("a script with an extension: %q %v", got, err)
	}
	got, err := bundleEntry(filepath.Join(dir, "cli"), "")
	if err != nil || got != filepath.Join(dir, "cli.cjs") {
		t.Fatalf("extensionless: %q %v", got, err)
	}
	if b, _ := os.ReadFile(got); string(b) != "#!/usr/bin/env node\nrequire('./lib')\n" {
		t.Errorf("the copy's bytes: %q", b)
	}
	if got, err := bundleEntry(filepath.Join(dir, "cli"), "module"); err != nil || got != filepath.Join(dir, "cli.mjs") {
		t.Errorf("type module: %q %v", got, err)
	}
}

// fakePlugin builds testdata/fakeplugin once per test binary: a
// plugin answering as PROBE_MODE says.
func fakePlugin(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "fakeplugin")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", exe, "./testdata/fakeplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the fake plugin: %v\n%s", err, out)
	}
	return exe
}

// modeScript wraps the fake plugin in a script setting its mode, the
// executable then bare-named as a tree's entrypoint is.
func modeScript(t *testing.T, dir, name, exe, mode string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	var body string
	if runtime.GOOS == "windows" {
		p += ".cmd"
		body = "@echo off\r\nset PROBE_MODE=" + mode + "\r\n\"" + exe + "\"\r\n"
	} else {
		body = "#!/bin/sh\nPROBE_MODE=" + mode + " exec " + exe + "\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Probe holds a built executable to the plugin protocol: a response
// with a file passes; nothing written, bytes no response parses
// from, a non-zero exit, an error of the plugin's own, and an empty
// response from a plugin not marked silent are refused; a silent
// plugin's response holding no file but its features passes, and
// nothing written is refused silent or not.
func TestProbe(t *testing.T) {
	exe := fakePlugin(t)
	dir := t.TempDir()
	ctx := context.Background()
	for _, c := range []struct {
		mode   string
		silent bool
		want   string
	}{
		{"file", false, ""}, {"file", true, ""}, {"error", false, "answered the probe with an error: go_package missing"}, {"error", true, "answered the probe with an error"},
		{"features", false, "no file"}, {"features", true, ""}, {"empty", false, "wrote nothing"}, {"empty", true, "wrote nothing"},
		{"garbage", false, "no plugin response"}, {"exit3", false, "exit status 3: boom"},
	} {
		err := Probe(ctx, modeScript(t, dir, c.mode, exe, c.mode), c.silent)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s silent=%v: %v, want %q", c.mode, c.silent, err, c.want)
		}
	}
	// The executable itself, bare-named as a tree's entrypoint, starts
	// on every host (windows through a copy under .exe).
	bare := filepath.Join(dir, "protoc-gen-bare")
	if err := copyFile(exe, bare); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROBE_MODE", "file")
	if err := Probe(ctx, bare, false); err != nil {
		t.Errorf("a bare-named executable: %v", err)
	}
}

// probeTree probes the host's tree alone: a build of other platforms
// is handed on unprobed, a build including the host's is held.
func TestProbeTree(t *testing.T) {
	exe := fakePlugin(t)
	out := t.TempDir()
	p := &catalog.Plugin{Entrypoint: "protoc-gen-x"}
	other := "linux/arm64"
	if Host() == other {
		other = "linux/amd64"
	}
	if err := probeTree(context.Background(), p, []string{other}, out); err != nil {
		t.Errorf("no host tree: %v", err)
	}
	if err := probeTree(context.Background(), p, []string{other, Host()}, out); err == nil {
		t.Error("a missing host tree probed as fine")
	}
	t.Setenv("PROBE_MODE", "empty")
	if err := copyFile(exe, filepath.Join(TreeDir(out, Host()), p.Entrypoint)); err != nil {
		t.Fatal(err)
	}
	if err := probeTree(context.Background(), p, []string{Host()}, out); err == nil || !strings.Contains(err.Error(), "wrote nothing") {
		t.Errorf("an empty answer: %v", err)
	}
	t.Setenv("PROBE_MODE", "file")
	if err := probeTree(context.Background(), p, []string{Host()}, out); err != nil {
		t.Errorf("a file answered: %v", err)
	}
}

// Build holds what a kind built to the probe: a kind whose tree
// answers nothing fails the build, one whose tree answers with a
// file passes, and a build of no host platform is handed on as the
// kind made it.
func TestBuildProbes(t *testing.T) {
	exe := fakePlugin(t)
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindGo, Entrypoint: "protoc-gen-x", Platforms: catalog.Platforms}}}
	saved := builders[catalog.KindGo]
	defer func() { builders[catalog.KindGo] = saved }()
	builders[catalog.KindGo] = func(_ context.Context, _ *catalog.Catalog, _ string, p *catalog.Plugin, _ string, platforms []string, out string) error {
		for _, pl := range platforms {
			if err := copyFile(exe, filepath.Join(TreeDir(out, pl), p.Entrypoint)); err != nil {
				return err
			}
		}
		return nil
	}
	other := "linux/arm64"
	if Host() == other {
		other = "linux/amd64"
	}
	t.Setenv("PROBE_MODE", "empty")
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "wrote nothing") {
		t.Errorf("a tree answering nothing built: %v", err)
	}
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{other}, t.TempDir()); err != nil {
		t.Errorf("no host tree, yet refused: %v", err)
	}
	t.Setenv("PROBE_MODE", "file")
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host(), other}, t.TempDir()); err != nil {
		t.Errorf("a tree answering a file refused: %v", err)
	}
}

// A go recipe's tags reach the build as one -tags argument, and none
// where it has none.
func TestGoBuildArgs(t *testing.T) {
	if got := strings.Join(goBuildArgs(nil, "o", "m/p"), " "); got != "build -trimpath -ldflags=-s -w -buildid= -o o m/p" {
		t.Errorf("no tags: %q", got)
	}
	if got := strings.Join(goBuildArgs([]string{"a", "b"}, "o", "m/p"), " "); got != "build -trimpath -ldflags=-s -w -buildid= -o o -tags a,b m/p" {
		t.Errorf("tags: %q", got)
	}
}
