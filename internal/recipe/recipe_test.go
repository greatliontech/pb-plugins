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
	"path/filepath"
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
	if fi, err := os.Stat(filepath.Join(dir, "src", "bin", "x")); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("bin/x: %v %v", fi, err)
	}
	if target, err := os.Readlink(filepath.Join(dir, "src", "link")); err != nil || target != "bin/x" {
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
	if got, err := binScript(dir, "protoc-gen-es"); err != nil || got != filepath.Join(dir, "bin", "protoc-gen-es") {
		t.Errorf("map: %q %v", got, err)
	}
	if _, err := binScript(dir, "missing"); err == nil {
		t.Error("a name the map lacks resolved")
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"bin":"cli.js"}`), 0o644)
	if got, err := binScript(dir, "whatever"); err != nil || got != filepath.Join(dir, "cli.js") {
		t.Errorf("string: %q %v", got, err)
	}
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o644)
	if _, err := binScript(dir, "x"); err == nil {
		t.Error("no bin resolved")
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
