package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildRust installs the crate at the version with cargo on the
// platform itself — the host's — into a throwaway root, locked to
// the crate's own lockfile, which the crate must publish (cargo
// only warns where it does not, resolving afresh), and lays its
// executable down as the tree's entrypoint.
func buildRust(ctx context.Context, _ *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	if len(platforms) != 1 {
		return fmt.Errorf("%s: a rust recipe builds one platform, the host's", name)
	}
	pl := platforms[0]
	if err := crateLocked(ctx, p.Crate, version); err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "pb-plugins-rust-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	// The registry's sparse protocol and the retry count are cargo's
	// own defaults, set here so a host's configuration cannot change
	// them.
	env := []string{"CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse", "CARGO_NET_RETRY=3"}
	if err := run(ctx, root, env, "cargo", cargoInstallArgs(p.Crate, version, root)...); err != nil {
		return err
	}
	bin := p.Bin
	if bin == "" {
		bin = p.Entrypoint
	}
	return copyFile(filepath.Join(root, "bin", catalog.Expand(bin+"{exe}", version, pl)), filepath.Join(TreeDir(out, pl), p.Entrypoint))
}

// crateLocked refuses a crate whose archive at the version publishes
// no Cargo.lock: without one, cargo's `--locked` holds nothing. The
// crate's canonical name is read from the registry first — the
// registry answers any spelling of a name with the crate, but serves
// its archive under the canonical spelling alone.
func crateLocked(ctx context.Context, crate, version string) error {
	entry, err := web.Get(ctx, endpoints.Crates+"/api/v1/crates/"+crate, map[string]string{"User-Agent": endpoints.UserAgent})
	if err != nil {
		return err
	}
	var doc struct {
		Crate struct {
			Name string `json:"name"`
		} `json:"crate"`
	}
	if err := json.Unmarshal(entry, &doc); err != nil {
		return err
	}
	if doc.Crate.Name == "" {
		return fmt.Errorf("%s: the registry names no crate", crate)
	}
	number := strings.TrimPrefix(version, "v")
	body, err := web.Get(ctx, fmt.Sprintf("%s/api/v1/crates/%s/%s/download", endpoints.Crates, doc.Crate.Name, number), map[string]string{"User-Agent": endpoints.UserAgent})
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s %s publishes no Cargo.lock: nothing to lock the build to", crate, version)
		}
		if err != nil {
			return err
		}
		// The archive's one top-level directory is the crate at the
		// version, as the registry spells it.
		if dir, file, ok := strings.Cut(path.Clean(h.Name), "/"); ok && dir != "" && file == "Cargo.lock" {
			return nil
		}
	}
}

// cargoInstallArgs are cargo's arguments: the crate at the version's
// number, locked to its lockfile, into the root.
func cargoInstallArgs(crate, version, root string) []string {
	return []string{"install", crate, "--version", strings.TrimPrefix(version, "v"), "--locked", "--root", root}
}
