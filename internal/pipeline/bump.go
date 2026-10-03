package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"github.com/greatliontech/pb-plugins/internal/github"
	"github.com/greatliontech/pb-plugins/internal/web"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// Discover lists the versions upstream has for a plugin, each as pb
// spells a tag, in any order; what is no version as the catalog
// spells one (catalog.IsVersion) is dropped by the bump.
type Discover func(ctx context.Context, p *catalog.Plugin) ([]string, error)

// Bump appends to each plugin's versions file the stable versions
// upstream has above its highest listed, ascending, and returns them
// by plugin. Versions below the highest are never backfilled: what
// the catalog skipped stays skipped, and a version enters by a hand
// edit alone. A frozen plugin is passed over whole.
func Bump(ctx context.Context, c *catalog.Catalog, discover Discover) (map[string][]string, error) {
	added := map[string][]string{}
	for _, name := range c.Names() {
		p := c.Plugins[name]
		if p.Frozen {
			continue
		}
		found, err := discover(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		highest := ""
		if len(p.Versions) > 0 {
			highest = p.Versions[len(p.Versions)-1]
		}
		var fresh []string
		seen := map[string]bool{}
		for _, v := range found {
			if !catalog.IsVersion(v) || seen[v] {
				continue
			}
			if highest != "" && semver.Compare(v, highest) <= 0 {
				continue
			}
			seen[v] = true
			fresh = append(fresh, v)
		}
		if len(fresh) == 0 {
			continue
		}
		sort.Slice(fresh, func(i, j int) bool { return semver.Compare(fresh[i], fresh[j]) < 0 })
		if err := catalog.AppendVersions(c.VersionsPath(name), fresh); err != nil {
			return nil, err
		}
		p.Versions = append(p.Versions, fresh...)
		added[name] = fresh
	}
	return added, nil
}

// Proxy is the module proxy's base URL; a test points it at a fake.
var Proxy = "https://proxy.golang.org"

// Upstream discovers versions from where each kind's versions live:
// the module proxy for go — the repository's releases where the
// recipe builds from a tag — the npm registry for node, the GitHub
// releases of the repository for release and bazel.
func Upstream(ctx context.Context, p *catalog.Plugin) ([]string, error) {
	switch p.Kind {
	case catalog.KindGo:
		if p.Repository != "" {
			return releaseVersions(ctx, p.Repository, p.Tag)
		}
		return goVersions(ctx, p.Module)
	case catalog.KindNode:
		return npmVersions(ctx, p.Package)
	case catalog.KindRelease, catalog.KindBazel:
		return releaseVersions(ctx, p.Repository, p.Tag)
	}
	return nil, fmt.Errorf("unknown kind %q", p.Kind)
}

func goVersions(ctx context.Context, mod string) ([]string, error) {
	escaped, err := module.EscapePath(mod)
	if err != nil {
		return nil, err
	}
	body, err := web.Get(ctx, Proxy+"/"+escaped+"/@v/list", nil)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(body)), nil
}

func npmVersions(ctx context.Context, pkg string) ([]string, error) {
	body, err := web.Get(ctx, "https://registry.npmjs.org/"+pkg, map[string]string{"Accept": "application/vnd.npm.install-v1+json"})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	var vs []string
	for v := range doc.Versions {
		vs = append(vs, "v"+v)
	}
	return vs, nil
}

// releaseVersions lists the repository's releases, neither drafts
// nor prereleases, and reads a version from each tag through the
// recipe's tag template: the tag is `<prefix><version><suffix>`
// around `{version}`, and a tag of another shape is no version.
func releaseVersions(ctx context.Context, repo, template string) ([]string, error) {
	prefix, suffix, _ := strings.Cut(template, "{version}")
	releases, err := github.Releases(ctx, repo)
	if err != nil {
		return nil, err
	}
	var vs []string
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		if v, ok := TagVersion(r.Tag, prefix, suffix); ok {
			vs = append(vs, v)
		}
	}
	return vs, nil
}

// TagVersion reads pb's `v`-spelled version from an upstream tag of
// the shape `<prefix><number><suffix>`.
func TagVersion(tag, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) || len(tag) < len(prefix)+len(suffix) {
		return "", false
	}
	number := tag[len(prefix) : len(tag)-len(suffix)]
	if v := "v" + number; catalog.IsVersion(v) {
		return v, true
	}
	return "", false
}
