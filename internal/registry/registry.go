// Package registry answers what the registry holds: whether a tag
// exists, which platform image an index digest resolves to, and
// whether a digest carries cosign's signature tag. Credentials are the
// ambient credential store's, as pb's own pull uses them.
package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// Client reads a registry.
type Client struct {
	opts []remote.Option
}

// New makes a client over the ambient credential store.
func New() *Client {
	return &Client{opts: []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}}
}

// TagExists reports whether the reference's tag names a manifest.
// A registry answering not-found for the tag or the repository
// reports false; any other failure is an error, so an unreachable
// registry never reads as an absent tag.
func (c *Client) TagExists(ctx context.Context, reference string) (bool, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return false, err
	}
	_, err = remote.Head(ref, append(c.opts, remote.WithContext(ctx))...)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("%s: %w", reference, err)
}

// Digest is the digest of the manifest a reference names, as the
// registry answers it; a tag naming nothing is an error.
func (c *Client) Digest(ctx context.Context, reference string) (string, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(ref, append(c.opts, remote.WithContext(ctx))...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", reference, err)
	}
	return desc.Digest.String(), nil
}

// Signed reports whether a tag is published and its list's digest
// carries cosign's signature tag: what the plan takes as done. A tag
// published by a run that died before signing is not done, and the
// next run signs it.
func (c *Client) Signed(ctx context.Context, reference string) (bool, error) {
	ok, err := c.TagExists(ctx, reference)
	if err != nil || !ok {
		return false, err
	}
	digest, err := c.Digest(ctx, reference)
	if err != nil {
		return false, err
	}
	repo := reference
	if i := strings.LastIndex(reference, ":"); i > strings.LastIndex(reference, "/") {
		repo = reference[:i]
	}
	sig, err := SignatureTag(repo + "@" + digest)
	if err != nil {
		return false, err
	}
	return c.TagExists(ctx, sig)
}

// PlatformImage resolves an index reference, `<repository>@<digest>`,
// to the digest of its image for the platform: `<repository>@<image
// digest>`. The reference must name an index holding exactly one
// entry for the platform.
func (c *Client) PlatformImage(ctx context.Context, reference, platform string) (string, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return "", err
	}
	if _, ok := ref.(name.Digest); !ok {
		return "", fmt.Errorf("%s: no digest", reference)
	}
	idx, err := remote.Index(ref, append(c.opts, remote.WithContext(ctx))...)
	if err != nil {
		return "", fmt.Errorf("%s: %w", reference, err)
	}
	m, err := idx.IndexManifest()
	if err != nil {
		return "", err
	}
	os, arch, _ := strings.Cut(platform, "/")
	var found string
	for _, d := range m.Manifests {
		if d.Platform == nil || d.Platform.OS != os || d.Platform.Architecture != arch {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%s: two entries for %s", reference, platform)
		}
		found = d.Digest.String()
	}
	if found == "" {
		return "", fmt.Errorf("%s: no entry for %s", reference, platform)
	}
	return ref.Context().Name() + "@" + found, nil
}

// SignatureTag is cosign's signature tag for a digest reference:
// `<algorithm>-<hex>.sig` in the same repository.
func SignatureTag(reference string) (string, error) {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return "", err
	}
	d, ok := ref.(name.Digest)
	if !ok {
		return "", fmt.Errorf("%s: no digest", reference)
	}
	return ref.Context().Name() + ":" + strings.Replace(d.DigestStr(), ":", "-", 1) + ".sig", nil
}

func isNotFound(err error) bool {
	var te *transport.Error
	if errors.As(err, &te) {
		if te.StatusCode == http.StatusNotFound {
			return true
		}
		for _, e := range te.Errors {
			if e.Code == transport.ManifestUnknownErrorCode || e.Code == transport.NameUnknownErrorCode {
				return true
			}
		}
	}
	return false
}
