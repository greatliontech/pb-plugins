package recipe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/endpoints"
	"github.com/greatliontech/pb-plugins/internal/web"
)

// buildSwift fetches the repository's archive at the version's tag,
// builds the recipe's product with swift on the platform itself —
// the host's — resolved to the package's lockfile where it commits
// one (SwiftPM refuses to resolve afresh under
// --force-resolved-versions, so a committed Package.resolved holds;
// a package committing none resolves as its manifest allows, as
// upstream's own builds do), for the static Linux SDK's target on
// linux, and lays the executable down as the tree's entrypoint,
// held to load elsewhere than this host (layDown).
func buildSwift(ctx context.Context, _ *catalog.Catalog, name string, p *catalog.Plugin, version string, platforms []string, out string) error {
	if len(platforms) != 1 {
		return fmt.Errorf("%s: a swift recipe builds one platform, the host's", name)
	}
	pl := platforms[0]
	src, err := fetchTag(ctx, p, version, pl, "pb-plugins-swift-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	_, err = os.Stat(filepath.Join(src, "Package.resolved"))
	locked := err == nil
	args := swiftBuildArgs(p.Product, SwiftTarget(pl), locked)
	if err := run(ctx, src, nil, "swift", args...); err != nil {
		return err
	}
	bin, err := output(ctx, src, nil, "swift", append(args, "--show-bin-path")...)
	if err != nil {
		return err
	}
	return layDown(p.Kind, name, pl, filepath.Join(strings.TrimSpace(bin), catalog.Expand(p.Product+"{exe}", version, pl)), filepath.Join(TreeDir(out, pl), p.Entrypoint))
}

// swiftBuildArgs are swift's arguments: the product in release
// configuration, for the target where the platform names one — its
// symbols stripped by the linker there, a static executable
// carrying the runtime's being a third of its size without them —
// held to the package's lockfile where it commits one.
func swiftBuildArgs(product, target string, locked bool) []string {
	args := []string{"build", "-c", "release", "--product", product}
	if target != "" {
		args = append(args, "--swift-sdk", target, "-Xlinker", "-s")
	}
	if locked {
		args = append(args, "--force-resolved-versions")
	}
	return args
}

// SwiftTarget is the Swift SDK a platform's tree is built with where
// it is not the runner's own: on linux the static Linux SDK's
// target, whose executable is static and so runs natively on an OS
// sandbox row; darwin builds for the runner's own.
func SwiftTarget(platform string) string {
	switch platform {
	case "linux/amd64":
		return "x86_64-swift-linux-musl"
	case "linux/arm64":
		return "aarch64-swift-linux-musl"
	}
	return ""
}

// PinnedStaticSDK is StaticSDK held to the catalog's pin: the URL
// swift.org publishes the bundle at, refused where the checksum
// swift.org publishes is not the pinned one.
func PinnedStaticSDK(ctx context.Context, version, pin string) (url string, err error) {
	url, sum, err := StaticSDK(ctx, version)
	if err != nil {
		return "", err
	}
	if sum != pin {
		return "", fmt.Errorf("swift %s: swift.org publishes %s for the release's static Linux SDK, the catalog pins %s", version, sum, pin)
	}
	return url, nil
}

// StaticSDK is the static Linux SDK of the Swift release at the
// version, three components — the release tagged
// `swift-<version>-RELEASE`, or `swift-<major>.<minor>-RELEASE`
// where the patch is zero and swift.org tagged the release without
// it (`swift-6.3-RELEASE`; `swift-6.4.0-RELEASE` keeps it) — the
// bundle's URL and the checksum swift.org publishes for it, which
// `swift sdk install` verifies.
func StaticSDK(ctx context.Context, version string) (url, checksum string, err error) {
	body, err := web.Get(ctx, endpoints.SwiftOrg+"/api/v1/install/releases.json", map[string]string{"User-Agent": endpoints.UserAgent})
	if err != nil {
		return "", "", err
	}
	var releases []struct {
		Name      string `json:"name"`
		Tag       string `json:"tag"`
		Platforms []struct {
			Platform string `json:"platform"`
			Version  string `json:"version"`
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(body, &releases); err != nil {
		return "", "", err
	}
	// The pin is three components (the catalog holds it so).
	tags := map[string]bool{"swift-" + version + "-RELEASE": true}
	if short, ok := strings.CutSuffix(version, ".0"); ok {
		tags["swift-"+short+"-RELEASE"] = true
	}
	for _, r := range releases {
		if !tags[r.Tag] {
			continue
		}
		tag := r.Tag
		for _, p := range r.Platforms {
			if p.Platform == "static-sdk" && p.Version != "" && p.Checksum != "" {
				url := fmt.Sprintf("%s/%s/static-sdk/%s/%s_static-linux-%s.artifactbundle.tar.gz", endpoints.SwiftDownload, strings.ToLower(tag), tag, tag, p.Version)
				return url, p.Checksum, nil
			}
		}
		return "", "", fmt.Errorf("swift %s: swift.org lists no static Linux SDK for it", version)
	}
	return "", "", fmt.Errorf("swift %s: swift.org lists no such release", version)
}
