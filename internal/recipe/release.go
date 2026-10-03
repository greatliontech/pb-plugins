package recipe

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildRelease downloads, per platform, the asset the recipe names —
// a release asset of the repository, or a URL — verifies its digest
// where the recipe names one, and lays the executable down as the
// tree's entrypoint: the member extracted from an archive, the asset
// itself otherwise.
func buildRelease(ctx context.Context, p *catalog.Plugin, version string, platforms []string, out string) error {
	for _, pl := range platforms {
		outfile := filepath.Join(TreeDir(out, pl), p.Entrypoint)
		if err := fetchAsset(ctx, assetURL(p, version, pl), p.Checksum, catalog.Expand(p.MemberOf(pl), version, pl), pl, outfile); err != nil {
			return fmt.Errorf("%s: %w", pl, err)
		}
	}
	return nil
}

// assetURL is where a platform's asset is fetched from: the asset
// itself where the recipe names it by URL (the catalog admits https
// alone; a test's fake speaks http), else under the repository's
// release at the version's tag.
func assetURL(p *catalog.Plugin, version, platform string) string {
	asset := catalog.Expand(p.Assets[platform], version, platform)
	if strings.Contains(asset, "://") {
		return asset
	}
	return "https://github.com/" + p.Repository + "/releases/download/" + catalog.Expand(p.Tag, version, "") + "/" + asset
}

// fetchAsset fetches the asset at url, verifies it against the
// digest upstream publishes beside it where checksum names one, and
// writes the executable to outfile: the archive's member for a
// `.zip` or a gzip-compressed tar (`.tar.gz`, `.tgz`), the asset
// itself otherwise — held to be an executable of the platform by its
// header, so an archive of another shape, or another platform's
// executable, is refused rather than published as the entrypoint.
func fetchAsset(ctx context.Context, url, checksum, member, platform, outfile string) error {
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	tmp, size, err := download(ctx, url)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if checksum != "" {
		if err := verify(ctx, tmp, url, checksum); err != nil {
			return err
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var err2 error
	switch catalog.Archive(url) {
	case "zip":
		err2 = extractZip(tmp, size, member, outfile)
	case "tgz":
		err2 = extractTarMember(tmp, member, outfile)
	default:
		err2 = writeFile(outfile, tmp)
	}
	if err2 != nil {
		return err2
	}
	// What was laid down is held to be the platform's executable, by
	// its header; anything else is removed again.
	if err := checkExecutable(outfile, platform); err != nil {
		os.Remove(outfile)
		return fmt.Errorf("%s: %w", url, err)
	}
	return nil
}

// checkExecutable tells whether the file is an executable of the
// platform by its header: an ELF on linux, a Mach-O or a universal
// binary holding the architecture on darwin, a PE on windows, each
// for the platform's architecture.
func checkExecutable(path, platform string) error {
	goos, goarch := catalog.SplitPlatform(platform)
	var ok bool
	var format string
	switch goos {
	case "linux":
		f, err := elf.Open(path)
		if err != nil {
			return fmt.Errorf("no ELF executable of %s: %w", platform, err)
		}
		defer f.Close()
		format = f.Machine.String()
		ok = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch] == f.Machine
	case "darwin":
		want := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[goarch]
		if fat, err := macho.OpenFat(path); err == nil {
			defer fat.Close()
			for _, a := range fat.Arches {
				format += a.Cpu.String() + " "
				ok = ok || a.Cpu == want
			}
			break
		}
		f, err := macho.Open(path)
		if err != nil {
			return fmt.Errorf("no Mach-O executable of %s: %w", platform, err)
		}
		defer f.Close()
		format = f.Cpu.String()
		ok = f.Cpu == want
	case "windows":
		f, err := pe.Open(path)
		if err != nil {
			return fmt.Errorf("no PE executable of %s: %w", platform, err)
		}
		defer f.Close()
		format = fmt.Sprintf("machine %#x", f.Machine)
		ok = map[string]uint16{"amd64": pe.IMAGE_FILE_MACHINE_AMD64, "arm64": pe.IMAGE_FILE_MACHINE_ARM64}[goarch] == f.Machine
	}
	if !ok {
		return fmt.Errorf("no executable of %s (%s)", platform, strings.TrimSpace(format))
	}
	return nil
}

// download fetches the URL to a temporary file, returned open at its
// end with its size.
func download(ctx context.Context, url string) (*os.File, int64, error) {
	body, err := web.Open(ctx, url, nil)
	if err != nil {
		return nil, 0, err
	}
	defer body.Close()
	tmp, err := os.CreateTemp("", "pb-plugins-asset-")
	if err != nil {
		return nil, 0, err
	}
	size, err := io.Copy(tmp, body)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	return tmp, size, nil
}

// downloadHeld fetches the URL to a temporary file and holds it to
// the sha256 upstream publishes for it, the file returned open at
// its start with its size; publisher names upstream in the refusal.
func downloadHeld(ctx context.Context, url, sha, publisher string) (*os.File, int64, error) {
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	tmp, size, err := download(ctx, url)
	if err != nil {
		return nil, 0, err
	}
	if err := func() error {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		h := sha256.New()
		if _, err := io.Copy(h, tmp); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, sha) {
			return fmt.Errorf("%s: sha256 %s, %s publishes %s", url, got, publisher, sha)
		}
		_, err := tmp.Seek(0, io.SeekStart)
		return err
	}(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	return tmp, size, nil
}

// verify compares the file's digest with the one upstream publishes
// beside the asset: `sha256`, at the asset's URL with `.sha256`
// appended, the hex digest the first word of the file.
func verify(ctx context.Context, f *os.File, url, checksum string) error {
	var h hash.Hash
	switch checksum {
	case "sha256":
		h = sha256.New()
	case "sha1":
		h = sha1.New()
	default:
		return fmt.Errorf("checksum %q is none of sha256, sha1", checksum)
	}
	published, err := web.Get(ctx, url+"."+checksum, nil)
	if err != nil {
		return err
	}
	fields := strings.Fields(string(published))
	if len(fields) == 0 {
		return fmt.Errorf("%s.%s: empty", url, checksum)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, fields[0]) {
		return fmt.Errorf("%s: %s %s, upstream publishes %s", url, checksum, got, fields[0])
	}
	return nil
}

func extractZip(f *os.File, size int64, member, outfile string) error {
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return err
	}
	for _, e := range zr.File {
		if path.Clean(e.Name) != member || e.FileInfo().IsDir() {
			continue
		}
		r, err := e.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		return writeFile(outfile, r)
	}
	return fmt.Errorf("no member %s", member)
}

func extractTarMember(r io.Reader, member, outfile string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("no member %s", member)
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Clean(h.Name) != member {
			continue
		}
		return writeFile(outfile, tr)
	}
}
