package recipe

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
)

// buildRelease downloads, per platform, the release asset the recipe
// names and extracts the member from it as the tree's entrypoint.
func buildRelease(ctx context.Context, p *catalog.Plugin, version string, platforms []string, out string) error {
	tag := catalog.Expand(p.Tag, version, "")
	for _, pl := range platforms {
		asset := catalog.Expand(p.Assets[pl], version, pl)
		url := "https://github.com/" + p.Repository + "/releases/download/" + tag + "/" + asset
		member := catalog.Expand(p.Member, version, pl)
		outfile := filepath.Join(TreeDir(out, pl), p.Entrypoint)
		if err := extractURL(ctx, url, member, outfile); err != nil {
			return fmt.Errorf("%s: %w", pl, err)
		}
	}
	return nil
}

// extractURL fetches the archive at url and writes its member to
// outfile: a `.zip`, or a gzip-compressed tar (`.tar.gz`, `.tgz`).
func extractURL(ctx context.Context, url, member, outfile string) error {
	fmt.Fprintf(os.Stderr, "+ fetch %s\n", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp("", "pb-plugins-asset-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return err
	}
	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractZip(tmp, size, member, outfile)
	case strings.HasSuffix(url, ".tar.gz"), strings.HasSuffix(url, ".tgz"):
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractTarMember(tmp, member, outfile)
	}
	return fmt.Errorf("%s: neither a zip nor a tar.gz", url)
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
