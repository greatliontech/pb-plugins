package recipe

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/pluginpb"
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
		"/a.tar.gz": tarGz(t, map[string]string{"bin/": "", "bin/e": string(elfOf(elf.EM_X86_64)), "readme": "r"}, nil),
		"/a.zip":    zipped(t, map[string]string{"bin/e.exe": string(peOf(pe.IMAGE_FILE_MACHINE_AMD64)), "readme": "r"}),
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
	if err := fetchAsset(context.Background(), srv.URL+"/a.tar.gz", "", "bin/e", "linux/amd64", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, elfOf(elf.EM_X86_64)) {
		t.Errorf("tar member %q", b)
	}
	if err := fetchAsset(context.Background(), srv.URL+"/a.zip", "", "bin/e.exe", "windows/amd64", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, peOf(pe.IMAGE_FILE_MACHINE_AMD64)) {
		t.Errorf("zip member %q", b)
	}
	if err := fetchAsset(context.Background(), srv.URL+"/a.zip", "", "bin/other", "windows/amd64", out); err == nil || !strings.Contains(err.Error(), "no member") {
		t.Errorf("missing member: %v", err)
	}
	if err := fetchAsset(context.Background(), srv.URL+"/missing.zip", "", "bin/e", "linux/amd64", out); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing asset: %v", err)
	}
	if err := fetchAsset(context.Background(), srv.URL+"/a.tar.gz", "", "bin/e", "linux/amd64", filepath.Join(dir, "deep", "e")); err != nil {
		t.Errorf("a missing directory is created: %v", err)
	}
}

// A release asset named by URL is fetched there, its templates
// filled; one named by its file name under the repository's release
// at the version's tag.
func TestAssetURL(t *testing.T) {
	byURL := &catalog.Plugin{Assets: map[string]string{"windows/amd64": "https://h/{version}/p-{version}{exe}"}}
	if got := assetURL(byURL, "v1.2.0", "windows/amd64"); got != "https://h/1.2.0/p-1.2.0.exe" {
		t.Errorf("by URL: %s", got)
	}
	byName := &catalog.Plugin{Repository: "o/r", Tag: "v{version}", Assets: map[string]string{"linux/amd64": "p-{version}-linux.tar.gz"}}
	if got := assetURL(byName, "v1.2.0", "linux/amd64"); got != "https://github.com/o/r/releases/download/v1.2.0/p-1.2.0-linux.tar.gz" {
		t.Errorf("by name: %s", got)
	}
}

// elfOf, machoOf, fatOf and peOf are the smallest headers the
// standard library's parsers read as an executable of the machine.
func elfOf(machine elf.Machine) []byte {
	b := make([]byte, 64)
	copy(b, "\x7fELF")
	b[4], b[5], b[6] = 2, 1, 1 // 64-bit, little-endian, version 1
	binary.LittleEndian.PutUint16(b[16:], 2)
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64) // the header's size
	return b
}

// elfWithInterp is elfOf with one program header, PT_INTERP: a
// dynamically linked executable as the loader sees it.
func elfWithInterp(machine elf.Machine) []byte {
	b := append(elfOf(machine), make([]byte, 56)...)
	binary.LittleEndian.PutUint64(b[32:], 64) // the program headers' offset
	binary.LittleEndian.PutUint16(b[54:], 56) // one entry's size
	binary.LittleEndian.PutUint16(b[56:], 1)  // one entry
	binary.LittleEndian.PutUint32(b[64:], uint32(elf.PT_INTERP))
	return b
}

func machoOf(cpu macho.Cpu) []byte {
	b := make([]byte, 32)
	binary.LittleEndian.PutUint32(b, 0xfeedfacf)
	binary.LittleEndian.PutUint32(b[4:], uint32(cpu))
	binary.LittleEndian.PutUint32(b[12:], 2) // an executable
	return b
}

// machoLoading is machoOf with one LC_LOAD_DYLIB command per
// library named; a name prefixed `weak:` is bound by
// LC_LOAD_WEAK_DYLIB instead, its name at an offset past the
// command's fixed words, as the offset word allows.
func machoLoading(cpu macho.Cpu, libs ...string) []byte {
	var cmds []byte
	for _, lib := range libs {
		kind, off := uint32(macho.LoadCmdDylib), 24
		if strings.HasPrefix(lib, "weak:") {
			kind, lib, off = 0x80000018, strings.TrimPrefix(lib, "weak:"), 32
		}
		name := append([]byte(lib), 0)
		for len(name)%8 != 0 {
			name = append(name, 0)
		}
		cmd := make([]byte, off)
		binary.LittleEndian.PutUint32(cmd, kind)
		binary.LittleEndian.PutUint32(cmd[4:], uint32(off+len(name)))
		binary.LittleEndian.PutUint32(cmd[8:], uint32(off)) // the name's offset
		cmds = append(cmds, append(cmd, name...)...)
	}
	b := machoOf(cpu)
	binary.LittleEndian.PutUint32(b[16:], uint32(len(libs)))
	binary.LittleEndian.PutUint32(b[20:], uint32(len(cmds)))
	return append(b, cmds...)
}

// fatOfBodies is a universal executable over the thin ones given.
func fatOfBodies(thins ...[]byte) []byte {
	b := make([]byte, 8+20*len(thins))
	binary.BigEndian.PutUint32(b, 0xcafebabe)
	binary.BigEndian.PutUint32(b[4:], uint32(len(thins)))
	var bodies []byte
	for i, thin := range thins {
		cpu := binary.LittleEndian.Uint32(thin[4:])
		off := len(b) + len(bodies)
		binary.BigEndian.PutUint32(b[8+20*i:], cpu)
		binary.BigEndian.PutUint32(b[8+20*i+8:], uint32(off))
		binary.BigEndian.PutUint32(b[8+20*i+12:], uint32(len(thin)))
		bodies = append(bodies, thin...)
	}
	return append(b, bodies...)
}

func fatOf(cpus ...macho.Cpu) []byte {
	b := make([]byte, 8+20*len(cpus))
	binary.BigEndian.PutUint32(b, 0xcafebabe)
	binary.BigEndian.PutUint32(b[4:], uint32(len(cpus)))
	var bodies []byte
	for i, cpu := range cpus {
		thin := machoOf(cpu)
		off := len(b) + len(bodies)
		binary.BigEndian.PutUint32(b[8+20*i:], uint32(cpu))
		binary.BigEndian.PutUint32(b[8+20*i+8:], uint32(off))
		binary.BigEndian.PutUint32(b[8+20*i+12:], uint32(len(thin)))
		bodies = append(bodies, thin...)
	}
	return append(b, bodies...)
}

func peOf(machine uint16) []byte {
	b := make([]byte, 64+24+112)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 64)
	copy(b[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(b[68:], machine)
	binary.LittleEndian.PutUint16(b[84:], 112) // a PE32+ optional header with no data directories
	binary.LittleEndian.PutUint16(b[88:], 0x20b)
	return b
}

// An asset that is the executable itself lands as the entrypoint
// whole where its header is the platform's executable's, machine
// included — an archive of another shape, or another architecture's
// executable, bare or extracted, is refused with nothing left; a
// sha256 published beside an asset is verified before the asset is
// read, a mismatch or a missing digest refusing it with nothing
// written, the digest read as the first word of the sidecar in
// either case.
func TestFetchAssetBareAndVerified(t *testing.T) {
	exe := elfOf(elf.EM_X86_64)
	sum := sha256.Sum256(exe)
	archive := tarGz(t, map[string]string{"bin/": "", "bin/e": string(exe)}, nil)
	archiveSum := sha256.Sum256(archive)
	files := map[string][]byte{
		"/p-1.0.exe":        exe,
		"/p-1.0.exe.sha256": []byte(strings.ToUpper(hex.EncodeToString(sum[:])) + "  p-1.0.exe\n"),
		"/wrong.exe":        exe,
		"/wrong.exe.sha256": []byte(strings.Repeat("0", 64) + "\n"),
		"/nosum.exe":        exe,
		"/a.tar.gz":         archive,
		"/a.tar.gz.sha256":  []byte(hex.EncodeToString(archiveSum[:])),
		"/x.tar.xz":         []byte("\xfd7zXZ\x00 compressed"),
		"/arm.elf":          elfOf(elf.EM_AARCH64),
		"/mac":              machoOf(macho.CpuArm64),
		"/fat":              fatOf(macho.CpuAmd64, macho.CpuArm64),
		"/fat-amd":          fatOf(macho.CpuAmd64),
		"/win.exe":          peOf(pe.IMAGE_FILE_MACHINE_AMD64),
		"/win-arm.exe":      peOf(pe.IMAGE_FILE_MACHINE_ARM64),
		"/arm.zip":          zipped(t, map[string]string{"bin/e": string(elfOf(elf.EM_AARCH64))}),
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
	if err := fetchAsset(context.Background(), srv.URL+"/p-1.0.exe", "sha256", "", "linux/amd64", out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); !bytes.Equal(b, exe) {
		t.Errorf("the bare asset: %q", b)
	}
	if fi, _ := os.Stat(out); runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
		t.Error("the bare asset is not executable")
	}
	if err := fetchAsset(context.Background(), srv.URL+"/wrong.exe", "sha256", "", "linux/amd64", filepath.Join(dir, "w")); err == nil || !strings.Contains(err.Error(), "upstream publishes") {
		t.Errorf("a mismatched digest: %v", err)
	}
	if err := fetchAsset(context.Background(), srv.URL+"/nosum.exe", "sha256", "", "linux/amd64", filepath.Join(dir, "n")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing digest: %v", err)
	}
	refusals := map[string]struct{ asset, member, platform string }{
		"x":  {"/x.tar.xz", "", "linux/amd64"},
		"a":  {"/arm.elf", "", "linux/amd64"},
		"m":  {"/mac", "", "linux/arm64"},
		"f":  {"/fat-amd", "", "darwin/arm64"},
		"wa": {"/win-arm.exe", "", "windows/amd64"},
		"z":  {"/arm.zip", "bin/e", "linux/amd64"},
	}
	for name, r := range refusals {
		if err := fetchAsset(context.Background(), srv.URL+r.asset, "", r.member, r.platform, filepath.Join(dir, name)); err == nil || !strings.Contains(err.Error(), "no executable of "+r.platform) && !strings.Contains(err.Error(), "executable of "+r.platform) {
			t.Errorf("%s refused: %v", r.asset, err)
		}
	}
	for _, refused := range []string{"w", "n", "x", "a", "m", "f", "wa", "z"} {
		if _, err := os.Stat(filepath.Join(dir, refused)); err == nil {
			t.Errorf("a refused asset was left: %s", refused)
		}
	}
	admissions := map[string]struct{ asset, platform string }{
		"mac":  {"/mac", "darwin/arm64"},
		"fat1": {"/fat", "darwin/amd64"},
		"fat2": {"/fat", "darwin/arm64"},
		"win":  {"/win.exe", "windows/amd64"},
		"wa2":  {"/win-arm.exe", "windows/arm64"},
		"arm":  {"/arm.elf", "linux/arm64"},
	}
	for name, a := range admissions {
		if err := fetchAsset(context.Background(), srv.URL+a.asset, "", "", a.platform, filepath.Join(dir, name)); err != nil {
			t.Errorf("%s for %s: %v", a.asset, a.platform, err)
		}
	}
	if err := fetchAsset(context.Background(), srv.URL+"/a.tar.gz", "sha256", "bin/e", "linux/amd64", filepath.Join(dir, "t")); err != nil {
		t.Fatalf("a verified archive: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "t")); !bytes.Equal(b, exe) {
		t.Errorf("the verified archive's member: %q", b)
	}
}

// A release recipe lays one tree per platform from the asset each
// names, the member a platform's own where the recipe names one.
func TestBuildReleaseMembers(t *testing.T) {
	files := map[string][]byte{
		"/v1.0.0/linux.tar.gz":  tarGz(t, map[string]string{"bin/": "", "bin/e": string(elfOf(elf.EM_X86_64))}, nil),
		"/v1.0.0/darwin.tar.gz": tarGz(t, map[string]string{"x64/": "", "x64/e": string(machoOf(macho.CpuAmd64))}, nil),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p := &catalog.Plugin{Kind: catalog.KindRelease, Entrypoint: "e", Member: "bin/e", Members: map[string]string{"darwin/amd64": "x64/e"}, Assets: map[string]string{
		"linux/amd64":  srv.URL + "/v{version}/linux.tar.gz",
		"darwin/amd64": srv.URL + "/v{version}/darwin.tar.gz",
	}}
	out := t.TempDir()
	if err := buildRelease(context.Background(), p, "v1.0.0", []string{"linux/amd64", "darwin/amd64"}, out); err != nil {
		t.Fatal(err)
	}
	for pl, want := range map[string][]byte{"linux/amd64": elfOf(elf.EM_X86_64), "darwin/amd64": machoOf(macho.CpuAmd64)} {
		if b, _ := os.ReadFile(filepath.Join(TreeDir(out, pl), "e")); !bytes.Equal(b, want) {
			t.Errorf("%s: %q", pl, b)
		}
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
	if err := Build(context.Background(), c, "connectrpc/rust", "v0.9.0", []string{other}, out); err == nil || !strings.Contains(err.Error(), "builds on the platform itself") {
		t.Errorf("rust kind off-host: %v", err)
	}
	if err := Build(context.Background(), c, "apple/swift", "v1.38.1", []string{other}, out); err == nil || !strings.Contains(err.Error(), "builds on the platform itself") {
		t.Errorf("swift kind off-host: %v", err)
	}
	if err := Build(context.Background(), c, "protocolbuffers/dart", "v25.1.0", []string{other}, out); err == nil || !strings.Contains(err.Error(), "builds on the platform itself") {
		t.Errorf("dart kind off-host: %v", err)
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

// The release kind reads beyond GitHub releases: grpc/java's
// executable from Maven Central, its sha256 verified, grpc/node's
// from grpc's binary host and ScalaPB's from its GitHub release,
// every platform each serves built from this host (network). Runs
// where PBPLUGINS_LIVE is set.
func TestReleaseSourcesLive(t *testing.T) {
	if os.Getenv("PBPLUGINS_LIVE") == "" {
		t.Skip("PBPLUGINS_LIVE unset")
	}
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{"grpc/java": "v1.84.0", "grpc/node": "v1.13.1", "community/scalapb-scala": "v0.11.17"} {
		out := t.TempDir()
		platforms := c.Plugins[name].PlatformsOf()
		if err := Build(context.Background(), c, name, version, platforms, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, pl := range platforms {
			if fi, err := os.Stat(filepath.Join(TreeDir(out, pl), c.Plugins[name].Entrypoint)); err != nil || fi.Size() == 0 {
				t.Errorf("%s %s: %v", name, pl, err)
			}
		}
	}
}

// cargo installs the crate at the version's number, locked, into
// the root.
func TestCargoInstallArgs(t *testing.T) {
	if got := strings.Join(cargoInstallArgs("connectrpc-codegen", "v0.9.0", "/r", ""), " "); got != "install connectrpc-codegen --version 0.9.0 --locked --root /r" {
		t.Errorf("cargo args: %s", got)
	}
	if got := strings.Join(cargoInstallArgs("c", "v1.0.0", "/r", "x86_64-unknown-linux-musl"), " "); got != "install c --version 1.0.0 --locked --root /r --target x86_64-unknown-linux-musl" {
		t.Errorf("cargo args with a target: %s", got)
	}
	// linux builds for the musl target, static; the others for the
	// runner's own; the rust kind alone adds a target.
	for pl, want := range map[string]string{"linux/amd64": "x86_64-unknown-linux-musl", "linux/arm64": "aarch64-unknown-linux-musl", "darwin/arm64": "", "windows/amd64": ""} {
		if got := RustTarget(pl); got != want {
			t.Errorf("target of %s: %q, want %q", pl, got, want)
		}
		if got := Target(catalog.KindRust, pl); got != want {
			t.Errorf("the rust kind's target of %s: %q, want %q", pl, got, want)
		}
		if got := Target(catalog.KindSwift, pl); got != SwiftTarget(pl) {
			t.Errorf("the swift kind's target of %s: %q, want %q", pl, got, SwiftTarget(pl))
		}
		for _, k := range []catalog.Kind{catalog.KindGo, catalog.KindBazel, catalog.KindNode, catalog.KindRelease} {
			if got := Target(k, pl); got != "" {
				t.Errorf("a target for the %s kind on %s: %q", k, pl, got)
			}
		}
	}
}

// A crate whose archive publishes no Cargo.lock is refused before
// cargo runs; one with it passes.
func TestCrateLocked(t *testing.T) {
	// As crates.io does: the crate's entry answers any spelling of
	// its name with the canonical one; the archive is served under
	// the canonical spelling alone.
	files := map[string][]byte{
		"/api/v1/crates/lock-ed/1.0.0/download":  tarGz(t, map[string]string{"lock-ed-1.0.0/Cargo.toml": "[package]", "lock-ed-1.0.0/Cargo.lock": "# lock"}, nil),
		"/api/v1/crates/unlocked/1.0.0/download": tarGz(t, map[string]string{"unlocked-1.0.0/Cargo.toml": "[package]"}, nil),
	}
	canonical := map[string]string{"lock-ed": "lock-ed", "Lock-ed": "lock-ed", "lock_ed": "lock-ed", "unlocked": "unlocked"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "pb-plugins") {
			http.Error(w, "name yourself", http.StatusForbidden)
			return
		}
		if name, ok := canonical[strings.TrimPrefix(r.URL.Path, "/api/v1/crates/")]; ok {
			fmt.Fprintf(w, `{"crate":{"name":%q}}`, name)
			return
		}
		if b, ok := files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	saved := endpoints.Crates
	endpoints.Crates = srv.URL
	defer func() { endpoints.Crates = saved }()
	if err := crateLocked(context.Background(), "lock-ed", "v1.0.0"); err != nil {
		t.Errorf("a locked crate: %v", err)
	}
	// Another spelling of the name: the registry's entry gives the
	// canonical one, under which the archive is read.
	for _, spelling := range []string{"Lock-ed", "lock_ed"} {
		if err := crateLocked(context.Background(), spelling, "v1.0.0"); err != nil {
			t.Errorf("a locked crate spelled %s: %v", spelling, err)
		}
	}
	if err := crateLocked(context.Background(), "unlocked", "v1.0.0"); err == nil || !strings.Contains(err.Error(), "publishes no Cargo.lock") {
		t.Errorf("an unlocked crate: %v", err)
	}
	if err := crateLocked(context.Background(), "missing", "v1.0.0"); err == nil {
		t.Error("a missing crate passed")
	}
}

// A rust build refuses an unlocked crate before cargo runs: with no
// cargo on the path, the refusal is the lock check's, not cargo's.
func TestBuildRustRefusesUnlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/crates/u":
			fmt.Fprint(w, `{"crate":{"name":"u"}}`)
		case "/api/v1/crates/u/1.0.0/download":
			w.Write(tarGz(t, map[string]string{"u-1.0.0/Cargo.toml": "[package]"}, nil))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	saved := endpoints.Crates
	endpoints.Crates = srv.URL
	defer func() { endpoints.Crates = saved }()
	t.Setenv("PATH", t.TempDir())
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindRust, Crate: "u", Entrypoint: "e", Platforms: []string{Host()}}}}
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "publishes no Cargo.lock") {
		t.Errorf("an unlocked crate: %v", err)
	}
}

// A linux tree of the rust kind is static: an ELF naming an
// interpreter is refused, one naming none passes.
func TestCheckStatic(t *testing.T) {
	dir := t.TempDir()
	static, dynamic := filepath.Join(dir, "s"), filepath.Join(dir, "d")
	os.WriteFile(static, elfOf(elf.EM_X86_64), 0o755)
	os.WriteFile(dynamic, elfWithInterp(elf.EM_X86_64), 0o755)
	if err := checkStatic(static); err != nil {
		t.Errorf("a static executable: %v", err)
	}
	if err := checkStatic(dynamic); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("a dynamic executable: %v", err)
	}
	if err := checkStatic(filepath.Join(dir, "none")); err == nil {
		t.Error("a missing file passed")
	}
}

// A rust build on linux refuses a dynamically linked executable
// cargo produced, laying nothing down; elsewhere no target is
// named and the check does not apply.
func TestBuildRustRefusesDynamic(t *testing.T) {
	if RustTarget(Host()) == "" {
		t.Skip("no target on this host")
	}
	if runtime.GOOS == "windows" {
		t.Skip("cargo is a shell script here")
	}
	srv := lockedCrate(t)
	defer srv.Close()
	bin := t.TempDir()
	dynamic := filepath.Join(bin, "dynamic")
	os.WriteFile(dynamic, elfWithInterp(map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]), 0o755)
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --root ]; then root=$2; fi; shift; done\nmkdir -p \"$root/bin\" && cp " + dynamic + " \"$root/bin/e\"\n"
	if err := os.WriteFile(filepath.Join(bin, "cargo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindRust, Crate: "u", Entrypoint: "e", Platforms: []string{Host()}}}}
	out := t.TempDir()
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, out); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("a dynamic executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(TreeDir(out, Host()), "e")); err == nil {
		t.Error("the dynamic executable was laid down")
	}
}

// lockedCrate serves the crate u at v1.0.0 with a lockfile, as the
// registry would.
func lockedCrate(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/crates/u":
			fmt.Fprint(w, `{"crate":{"name":"u"}}`)
		case "/api/v1/crates/u/1.0.0/download":
			w.Write(tarGz(t, map[string]string{"u-1.0.0/Cargo.toml": "[package]", "u-1.0.0/Cargo.lock": "# lock"}, nil))
		default:
			http.NotFound(w, r)
		}
	}))
	saved := endpoints.Crates
	endpoints.Crates = srv.URL
	t.Cleanup(func() { endpoints.Crates = saved })
	return srv
}

// A rust build hands cargo the host platform's target: the musl one
// on linux, where the executable is to be static, none elsewhere;
// cargo here a script recording its arguments and laying the
// executable down where the build reads it.
func TestBuildRustTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cargo is a shell script here")
	}
	srv := lockedCrate(t)
	defer srv.Close()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	// The root follows --root; the executable, the fake plugin so
	// the probe is answered, lands under its bin.
	exe := fakePlugin(t)
	t.Setenv("PROBE_MODE", "file")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --root ]; then root=$2; fi; shift; done\nmkdir -p \"$root/bin\" && cp " + exe + " \"$root/bin/e\"\n"
	if err := os.WriteFile(filepath.Join(bin, "cargo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindRust, Crate: "u", Entrypoint: "e", Platforms: []string{Host()}}}}
	out := t.TempDir()
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, out); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "install u --version 1.0.0 --locked --root "
	if !strings.HasPrefix(string(args), want) {
		t.Errorf("cargo's arguments: %q", args)
	}
	target := RustTarget(Host())
	if has := strings.Contains(string(args), " --target "); has != (target != "") || (target != "" && !strings.HasSuffix(strings.TrimSpace(string(args)), " --target "+target)) {
		t.Errorf("cargo's target on %s: %q, want %q", Host(), args, target)
	}
	if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), "e")); err != nil || fi.Size() == 0 {
		t.Errorf("the tree: %v", err)
	}
}

// swift builds the product in release configuration, for the static
// Linux SDK's target on linux with its symbols stripped, held to the
// package's lockfile where it commits one.
func TestSwiftBuildArgs(t *testing.T) {
	if got := strings.Join(swiftBuildArgs("protoc-gen-swift", "", false), " "); got != "build -c release --product protoc-gen-swift" {
		t.Errorf("swift args: %s", got)
	}
	if got := strings.Join(swiftBuildArgs("e", "x86_64-swift-linux-musl", true), " "); got != "build -c release --product e --swift-sdk x86_64-swift-linux-musl -Xlinker -s --force-resolved-versions" {
		t.Errorf("swift args with a target and a lockfile: %s", got)
	}
	for pl, want := range map[string]string{"linux/amd64": "x86_64-swift-linux-musl", "linux/arm64": "aarch64-swift-linux-musl", "darwin/arm64": "", "darwin/amd64": "", "windows/amd64": ""} {
		if got := SwiftTarget(pl); got != want {
			t.Errorf("target of %s: %q, want %q", pl, got, want)
		}
	}
}

// The static Linux SDK of a Swift release is the bundle swift.org
// lists for it, found by its tag, at the revision and checksum it
// publishes; a release swift.org does not list, or lists without
// the SDK, is refused.
func TestStaticSDK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/install/releases.json" || !strings.Contains(r.Header.Get("User-Agent"), "pb-plugins") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `[{"name":"6.3","tag":"swift-6.3-RELEASE","platforms":[{"name":"Static SDK","platform":"static-sdk","version":"0.1.0","checksum":"63aa"}]},{"name":"6.3.3","tag":"swift-6.3.3-RELEASE","platforms":[{"name":"Ubuntu 24.04","platform":"Linux"}]},{"name":"6.4.0","tag":"swift-6.4.0-RELEASE","platforms":[{"name":"Ubuntu 24.04","platform":"Linux"},{"name":"Wasm SDK","platform":"wasm-sdk","version":"0.1.0","checksum":"f07b"},{"name":"Static SDK","platform":"static-sdk","version":"0.1.0","checksum":"47d2"}]}]`)
	}))
	defer srv.Close()
	saved := endpoints.SwiftOrg
	endpoints.SwiftOrg = srv.URL
	defer func() { endpoints.SwiftOrg = saved }()
	url, sum, err := StaticSDK(context.Background(), "6.4.0")
	if err != nil || sum != "47d2" || url != "https://download.swift.org/swift-6.4.0-release/static-sdk/swift-6.4.0-RELEASE/swift-6.4.0-RELEASE_static-linux-0.1.0.artifactbundle.tar.gz" {
		t.Errorf("the SDK: %s %s %v", url, sum, err)
	}
	// A release swift.org tagged without its zero patch is found
	// from the three-component pin all the same.
	url, sum, err = StaticSDK(context.Background(), "6.3.0")
	if err != nil || sum != "63aa" || url != "https://download.swift.org/swift-6.3-release/static-sdk/swift-6.3-RELEASE/swift-6.3-RELEASE_static-linux-0.1.0.artifactbundle.tar.gz" {
		t.Errorf("the SDK of a release tagged without a patch: %s %s %v", url, sum, err)
	}
	if _, _, err := StaticSDK(context.Background(), "6.3.1"); err == nil || !strings.Contains(err.Error(), "no such release") {
		t.Errorf("a patch release swift.org does not list: %v", err)
	}
	// The pinned SDK is swift.org's where the checksums agree, refused
	// where they do not.
	if url, err := PinnedStaticSDK(context.Background(), "6.4.0", "47d2"); err != nil || !strings.HasSuffix(url, "swift-6.4.0-RELEASE_static-linux-0.1.0.artifactbundle.tar.gz") {
		t.Errorf("the pinned SDK: %s %v", url, err)
	}
	if _, err := PinnedStaticSDK(context.Background(), "6.4.0", "0000"); err == nil || !strings.Contains(err.Error(), "the catalog pins 0000") {
		t.Errorf("a pin swift.org disagrees with: %v", err)
	}
	if _, _, err := StaticSDK(context.Background(), "6.3.3"); err == nil || !strings.Contains(err.Error(), "no static Linux SDK") {
		t.Errorf("a release without the SDK: %v", err)
	}
	if _, _, err := StaticSDK(context.Background(), "6.9.0"); err == nil || !strings.Contains(err.Error(), "no such release") {
		t.Errorf("a release swift.org does not list: %v", err)
	}
}

// A swift build fetches the repository's archive at the tag, hands
// swift the product, the host platform's target (the static Linux
// SDK's on linux, none elsewhere) and the lockfile flag where the
// package commits one, reads the executable where swift says it
// lies and lays it down; swift here a script recording its
// arguments and laying the fake plugin down there.
func TestBuildSwiftTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("swift is a shell script here")
	}
	archives := map[string][]byte{
		"/o/r/archive/refs/tags/1.0.0.tar.gz": tarGz(t, map[string]string{"r-1.0.0/Package.swift": "// swift-tools-version:6.0", "r-1.0.0/Package.resolved": "{}"}, nil),
		"/o/r/archive/refs/tags/2.0.0.tar.gz": tarGz(t, map[string]string{"r-2.0.0/Package.swift": "// swift-tools-version:6.0"}, nil),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := archives[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	saved := endpoints.GitHub
	endpoints.GitHub = srv.URL
	defer func() { endpoints.GitHub = saved }()
	exe := fakePlugin(t)
	t.Setenv("PROBE_MODE", "file")
	log := fakeSwift(t, exe)
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindSwift, Repository: "o/r", Tag: "{version}", Product: "e", Entrypoint: "e", Platforms: []string{Host()}}}}
	for version, locked := range map[string]bool{"v1.0.0": true, "v2.0.0": false} {
		os.Remove(log)
		out := t.TempDir()
		if err := Build(context.Background(), c, "a/b", version, []string{Host()}, out); err != nil {
			t.Fatalf("%s: %v", version, err)
		}
		args, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		// The build, then the same build asked where its products lie.
		build := strings.Join(swiftBuildArgs("e", SwiftTarget(Host()), locked), " ")
		if want := build + "\n" + build + " --show-bin-path\n"; string(args) != want {
			t.Errorf("%s: swift's calls %q, want %q", version, args, want)
		}
		if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), "e")); err != nil || fi.Size() == 0 {
			t.Errorf("%s: the tree: %v", version, err)
		}
	}
}

// A swift build on linux refuses a dynamically linked executable
// swift produced, laying nothing down.
func TestBuildSwiftRefusesDynamic(t *testing.T) {
	if SwiftTarget(Host()) == "" {
		t.Skip("no target on this host")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarGz(t, map[string]string{"r-1.0.0/Package.swift": "// swift-tools-version:6.0"}, nil))
	}))
	defer srv.Close()
	saved := endpoints.GitHub
	endpoints.GitHub = srv.URL
	defer func() { endpoints.GitHub = saved }()
	dynamic := filepath.Join(t.TempDir(), "dynamic")
	os.WriteFile(dynamic, elfWithInterp(map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]), 0o755)
	fakeSwift(t, dynamic)
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindSwift, Repository: "o/r", Tag: "{version}", Product: "e", Entrypoint: "e", Platforms: []string{Host()}}}}
	out := t.TempDir()
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, out); err == nil || !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("a dynamic executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(TreeDir(out, Host()), "e")); err == nil {
		t.Error("the dynamic executable was laid down")
	}
}

// fakeSwift puts a swift on the PATH that records every call and,
// asked where the products lie, answers with the release directory,
// where a build lays exe down under the product's name; the log's
// path is returned.
func fakeSwift(t *testing.T, exe string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\ncase \"$*\" in *--show-bin-path*) echo \"$PWD/.build/release\"; exit 0;; esac\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --product ]; then prod=$2; fi; shift; done\nmkdir -p .build/release && cp " + exe + " \".build/release/$prod\"\n"
	if err := os.WriteFile(filepath.Join(bin, "swift"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// A darwin tree binds to the system's libraries alone: a Mach-O
// loading a library through a search path or from a toolchain is
// refused, thin or any architecture of a universal one; one loading
// the system's passes.
func TestCheckPortable(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	system := machoLoading(macho.CpuArm64, "/usr/lib/libSystem.B.dylib", "/usr/lib/swift/libswiftCore.dylib")
	rpath := machoLoading(macho.CpuArm64, "/usr/lib/libSystem.B.dylib", "@rpath/libswiftCompatibilitySpan.dylib")
	toolchain := machoLoading(macho.CpuAmd64, "/Library/Developer/Toolchains/swift-6.4.0-RELEASE.xctoolchain/usr/lib/swift/macosx/libswift_Concurrency.dylib")
	weak := machoLoading(macho.CpuArm64, "/usr/lib/libSystem.B.dylib", "weak:@rpath/libswiftCompatibilitySpan.dylib")
	weakSystem := machoLoading(macho.CpuArm64, "weak:/usr/lib/swift/libswift_Concurrency.dylib")
	if err := checkPortable(write("system", system)); err != nil {
		t.Errorf("the system's libraries: %v", err)
	}
	if err := checkPortable(write("rpath", rpath)); err == nil || !strings.Contains(err.Error(), "@rpath/libswiftCompatibilitySpan.dylib") {
		t.Errorf("a search path: %v", err)
	}
	if err := checkPortable(write("toolchain", toolchain)); err == nil || !strings.Contains(err.Error(), "/Library/Developer/Toolchains") {
		t.Errorf("a toolchain's library: %v", err)
	}
	// A weak binding counts as a binding: a toolchain's compatibility
	// library is bound so.
	if err := checkPortable(write("weak", weak)); err == nil || !strings.Contains(err.Error(), "@rpath/libswiftCompatibilitySpan.dylib") {
		t.Errorf("a weak search-path binding: %v", err)
	}
	if err := checkPortable(write("weak-system", weakSystem)); err != nil {
		t.Errorf("a weak binding to the system: %v", err)
	}
	// A binding whose name cannot be read is refused, not passed over.
	malformed := machoLoading(macho.CpuArm64, "weak:/usr/lib/libSystem.B.dylib")
	binary.LittleEndian.PutUint32(malformed[32+8:], 0xffff) // the name's offset, outside the command
	if err := checkPortable(write("malformed", malformed)); err == nil || !strings.Contains(err.Error(), "no readable library") {
		t.Errorf("a malformed binding: %v", err)
	}
	// A command too short for a binding's fixed words is refused too.
	short := machoOf(macho.CpuArm64)
	short = append(short, 0x18, 0, 0, 0x80, 8, 0, 0, 0) // LC_LOAD_WEAK_DYLIB, 8 bytes long
	binary.LittleEndian.PutUint32(short[16:], 1)
	binary.LittleEndian.PutUint32(short[20:], 8)
	if err := checkPortable(write("short", short)); err == nil || !strings.Contains(err.Error(), "no readable library") {
		t.Errorf("a binding too short for its words: %v", err)
	}
	inHeader := machoLoading(macho.CpuArm64, "weak:/usr/lib/libSystem.B.dylib")
	binary.LittleEndian.PutUint32(inHeader[32+8:], 8) // the name's offset, inside the fixed words
	if err := checkPortable(write("in-header", inHeader)); err == nil || !strings.Contains(err.Error(), "no readable library") {
		t.Errorf("a binding named inside the command's fixed words: %v", err)
	}
	if err := checkPortable(write("fat", fatOfBodies(system, toolchain))); err == nil || !strings.Contains(err.Error(), "/Library/Developer/Toolchains") {
		t.Errorf("a universal executable with one architecture bound to a toolchain: %v", err)
	}
	if err := checkPortable(write("none", []byte("x"))); err == nil {
		t.Error("no Mach-O passed")
	}
}

// An executable is laid down as a platform's entrypoint once held to
// load elsewhere than the runner: a linux one static where its kind
// takes no base, a darwin one bound to the system's libraries, a
// windows one as it is; a refused one is not laid down.
func TestLayDown(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name     string
		kind     catalog.Kind
		platform string
		built    []byte
		want     string
	}{
		{"static elf", catalog.KindSwift, "linux/arm64", elfOf(elf.EM_AARCH64), ""},
		{"dynamic elf", catalog.KindRust, "linux/amd64", elfWithInterp(elf.EM_X86_64), "dynamically linked"},
		{"dynamic elf of a kind over the base", catalog.KindBazel, "linux/amd64", elfWithInterp(elf.EM_X86_64), ""},
		{"system macho", catalog.KindSwift, "darwin/arm64", machoLoading(macho.CpuArm64, "/usr/lib/libSystem.B.dylib"), ""},
		{"rpath macho", catalog.KindBazel, "darwin/amd64", machoLoading(macho.CpuAmd64, "@rpath/libswift_Concurrency.dylib"), "@rpath/libswift_Concurrency.dylib"},
		{"windows as it is", catalog.KindRust, "windows/amd64", []byte("MZ"), ""},
	} {
		dst := filepath.Join(dir, tc.name, "e")
		err := layDown(tc.kind, "a/b", tc.platform, write(tc.name+".built", tc.built), dst)
		_, laid := os.Stat(dst)
		switch {
		case tc.want == "" && (err != nil || laid != nil):
			t.Errorf("%s: %v, laid down: %v", tc.name, err, laid == nil)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "a/b "+tc.platform) || laid == nil):
			t.Errorf("%s: %v, laid down: %v", tc.name, err, laid == nil)
		}
	}
}

// The probe's request carries its one file in both lists protoc
// fills, named to generate, with the recipe's parameter where one
// is given.
func TestProbeRequest(t *testing.T) {
	for _, parameter := range []string{"", "extern_path=.=crate::proto"} {
		b, err := probeRequest(parameter)
		if err != nil {
			t.Fatal(err)
		}
		var req pluginpb.CodeGeneratorRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.FileToGenerate) != 1 || len(req.ProtoFile) != 1 || len(req.SourceFileDescriptors) != 1 || req.ProtoFile[0].GetName() != req.FileToGenerate[0] || req.SourceFileDescriptors[0].GetName() != req.FileToGenerate[0] || req.GetParameter() != parameter {
			t.Errorf("the request with parameter %q: %v", parameter, &req)
		}
		if len(req.SourceFileDescriptors[0].Service) != 1 || len(req.SourceFileDescriptors[0].MessageType) != 2 {
			t.Errorf("the file's shapes: %v", req.SourceFileDescriptors[0])
		}
	}
}

// dart compiles the script to a standalone executable at the path
// given.
func TestDartCompileArgs(t *testing.T) {
	if got := strings.Join(dartCompileArgs("bin/m.dart", "/o/e"), " "); got != "compile exe bin/m.dart -o /o/e" {
		t.Errorf("dart args: %s", got)
	}
}

// A dart build fetches the repository's archive at the tag, resolves
// the package in its directory, compiles the script to the
// entrypoint's name and lays it down; dart here a script recording
// its calls and laying the fake plugin down at the output path.
func TestBuildDart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("dart is a shell script here")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/o/r/archive/refs/tags/p-v1.0.0.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(tarGz(t, map[string]string{"r-p-v1.0.0/pubspec.yaml": "name: w", "r-p-v1.0.0/packages/p/pubspec.yaml": "name: p", "r-p-v1.0.0/packages/p/bin/m.dart": "void main() {}"}, nil))
	}))
	defer srv.Close()
	saved := endpoints.GitHub
	endpoints.GitHub = srv.URL
	defer func() { endpoints.GitHub = saved }()
	exe := fakePlugin(t)
	t.Setenv("PROBE_MODE", "file")
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	// Every call is recorded with the directory it runs in; a
	// compile lays the fake plugin down at -o.
	script := "#!/bin/sh\nprintf '%s: %s\\n' \"$(basename \"$(dirname \"$PWD\")\")/$(basename \"$PWD\")\" \"$*\" >> " + log + "\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then cp " + exe + " \"$2\"; fi; shift; done\n"
	if err := os.WriteFile(filepath.Join(bin, "dart"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := &catalog.Catalog{Plugins: map[string]*catalog.Plugin{"a/b": {Kind: catalog.KindDart, Repository: "o/r", Tag: "p-v{version}", Dir: "packages/p", Main: "bin/m.dart", Entrypoint: "e", Platforms: []string{Host()}}}}
	out := t.TempDir()
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, out); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || lines[0] != "packages/p: pub get" || !strings.HasPrefix(lines[1], "packages/p: compile exe bin/m.dart -o ") || !strings.HasSuffix(lines[1], string(filepath.Separator)+"e") {
		t.Errorf("dart's calls: %q", lines)
	}
	if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), "e")); err != nil || fi.Size() == 0 {
		t.Errorf("the tree: %v", err)
	}
}

// The swift and dart kinds build every plugin of theirs that serves
// this host, at the versions the catalog's files hold, bufbuild's
// swift ones from the moved repository (network: GitHub, the
// packages' dependencies; the kind's toolchain, swift with the
// static Linux SDK installed on linux): the versions the catalog
// holds for the host, no fewer than the kind's floor here — swift
// serves no windows, dart every platform — so a test building
// nothing is caught. Runs where PBPLUGINS_LIVE is set.
func TestTagKindsLive(t *testing.T) {
	if os.Getenv("PBPLUGINS_LIVE") == "" {
		t.Skip("PBPLUGINS_LIVE unset")
	}
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	floors := map[catalog.Kind]int{catalog.KindSwift: 7, catalog.KindDart: 2}
	if runtime.GOOS == "windows" {
		floors[catalog.KindSwift] = 0
	}
	for _, kind := range []catalog.Kind{catalog.KindSwift, catalog.KindDart} {
		served, built := 0, 0
		for _, name := range c.Names() {
			p := c.Plugins[name]
			if p.Kind != kind || !slices.Contains(p.PlatformsOf(), Host()) {
				continue
			}
			served += len(p.Versions)
			for _, version := range p.Versions {
				out := t.TempDir()
				if err := Build(context.Background(), c, name, version, []string{Host()}, out); err != nil {
					t.Errorf("%s %s: %v", name, version, err)
					continue
				}
				if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), p.Entrypoint)); err != nil || fi.Size() == 0 {
					t.Errorf("%s %s: the tree: %v", name, version, err)
				}
				built++
			}
		}
		if built != served || served < floors[kind] {
			t.Errorf("%s: %d of %d versions serving this host built, the floor here %d", kind, built, served, floors[kind])
		}
	}
}

// The rust kind installs connectrpc/rust's crate on this host and
// lays its executable down (network: crates.io; cargo, with the
// musl target installed on linux). Runs where PBPLUGINS_LIVE is set.
func TestRustKindLive(t *testing.T) {
	if os.Getenv("PBPLUGINS_LIVE") == "" {
		t.Skip("PBPLUGINS_LIVE unset")
	}
	c, err := catalog.Load("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := Build(context.Background(), c, "connectrpc/rust", "v0.9.0", []string{Host()}, out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(TreeDir(out, Host()), "protoc-gen-connect-rust")); err != nil || fi.Size() == 0 {
		t.Errorf("the tree: %v", err)
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
	// Static, as a tree's entrypoint is held to be on linux.
	cmd := exec.Command("go", "build", "-o", exe, "./testdata/fakeplugin")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
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
		mode      string
		parameter string
		silent    bool
		want      string
	}{
		{"file", "", false, ""}, {"file", "", true, ""}, {"error", "", false, "answered the probe with an error: go_package missing"}, {"error", "", true, "answered the probe with an error"},
		{"features", "", false, "no file"}, {"features", "", true, ""}, {"empty", "", false, "wrote nothing"}, {"empty", "", true, "wrote nothing"},
		{"garbage", "", false, "no plugin response"}, {"exit3", "", false, "exit status 3: boom"},
		{"parameter", "", false, "parameter p=1 required"}, {"parameter", "p=1", false, ""},
	} {
		err := Probe(ctx, modeScript(t, dir, c.mode, exe, c.mode), c.parameter, c.silent)
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
	if err := Probe(ctx, bare, "", false); err != nil {
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
	// The recipe's parameter reaches the probe's request: a plugin
	// answering nothing without it is refused where the recipe names
	// none, built where it does.
	t.Setenv("PROBE_MODE", "parameter")
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "parameter p=1 required") {
		t.Errorf("a plugin needing a parameter built without one: %v", err)
	}
	c.Plugins["a/b"].Parameter = "p=1"
	if err := Build(context.Background(), c, "a/b", "v1.0.0", []string{Host()}, t.TempDir()); err != nil {
		t.Errorf("the recipe's parameter not carried: %v", err)
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
