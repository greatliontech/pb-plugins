package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/recipe"
)

// Registry is what publishing asks the registry: the platform image
// of the base's index, and whether a signature tag exists.
type Registry interface {
	PlatformImage(ctx context.Context, reference, platform string) (string, error)
	TagExists(ctx context.Context, reference string) (bool, error)
}

// Publisher publishes one plugin version from its trees.
type Publisher struct {
	Catalog  *catalog.Catalog
	Registry Registry
	// PB and Cosign are the executables run; `pb` and `cosign` on the
	// path where empty.
	PB, Cosign string
	// Trees holds the trees as recipe.Build laid them out.
	Trees string
	// Out receives the report.
	Out io.Writer
}

// Publish packages the trees of the plugin version into one list at
// its reference through `pb plugin build` (every platform the plugin
// serves must have its tree; a Linux tree of a kind needing the base
// is layered over the base's image for its platform), then signs the
// list and its images keyless with cosign where the list's digest
// carries no signature tag yet: a publish signed once is not signed
// again, and a tag published by a run that died before signing is
// signed by the next.
func (p *Publisher) Publish(ctx context.Context, name, version string) error {
	pl, ok := p.Catalog.Plugins[name]
	if !ok {
		return fmt.Errorf("%s: not in the catalog", name)
	}
	ref := p.Catalog.Reference(name, version)
	args := []string{"plugin", "build", ref, "--entrypoint", "/" + pl.Entrypoint}
	for _, platform := range pl.PlatformsOf() {
		tree := recipe.TreeDir(p.Trees, platform)
		if _, err := os.Stat(filepath.Join(tree, pl.Entrypoint)); err != nil {
			return fmt.Errorf("%s %s: no tree for %s: %w", name, version, platform, err)
		}
		args = append(args, "--platform", platform+"="+tree)
		if pl.Kind.NeedsBase() && strings.HasPrefix(platform, "linux/") {
			base, err := p.Registry.PlatformImage(ctx, p.Catalog.Base, platform)
			if err != nil {
				return fmt.Errorf("base for %s: %w", platform, err)
			}
			args = append(args, "--base", platform+"="+base)
		}
	}
	digest, report, err := p.build(ctx, args)
	if err != nil {
		return err
	}
	fmt.Fprint(p.Out, report)
	subject := strings.SplitN(ref, ":", 2)[0]
	if i := strings.LastIndex(ref, ":"); i > 0 {
		subject = ref[:i]
	}
	subject += "@" + digest
	signed, err := signatureExists(ctx, p.Registry, subject)
	if err != nil {
		return err
	}
	if signed {
		fmt.Fprintf(p.Out, "%s signed already\n", subject)
		return nil
	}
	cosign := p.Cosign
	if cosign == "" {
		cosign = "cosign"
	}
	// The legacy carrier, the signature tag, so the signature's
	// presence is a tag's; the bundle rides its annotations, so the
	// verification pb runs is offline.
	cmd := exec.CommandContext(ctx, cosign, "sign", "--yes", "--recursive", "--new-bundle-format=false", subject)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	fmt.Fprintf(os.Stderr, "+ %s\n", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cosign sign %s: %w", subject, err)
	}
	fmt.Fprintf(p.Out, "%s signed\n", subject)
	return nil
}

// build runs `pb plugin build` and reads the list's digest from its
// report, whose first line is `<reference>@<digest> published` or
// `... unchanged` (pb's plugin-publish contract).
func (p *Publisher) build(ctx context.Context, args []string) (digest, report string, err error) {
	pb := p.PB
	if pb == "" {
		pb = "pb"
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, pb, args...)
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	fmt.Fprintf(os.Stderr, "+ %s\n", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("pb plugin build: %w", err)
	}
	digest, err = ParseReport(out.String())
	if err != nil {
		return "", "", err
	}
	return digest, out.String(), nil
}

// ParseReport reads the list's digest from `pb plugin build`'s
// report.
func ParseReport(report string) (string, error) {
	first, _, _ := strings.Cut(report, "\n")
	fields := strings.Fields(first)
	if len(fields) != 2 || (fields[1] != "published" && fields[1] != "unchanged") {
		return "", fmt.Errorf("pb plugin build reported %q, not `<reference>@<digest> published|unchanged`", first)
	}
	_, digest, ok := strings.Cut(fields[0], "@")
	if !ok || !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("pb plugin build reported %q: no digest", first)
	}
	return digest, nil
}

func signatureExists(ctx context.Context, r Registry, subject string) (bool, error) {
	repo, digest, _ := strings.Cut(subject, "@")
	tag := repo + ":" + strings.Replace(digest, ":", "-", 1) + ".sig"
	return r.TagExists(ctx, tag)
}
