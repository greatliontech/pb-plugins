package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/catalog"
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
// edit alone.
func Bump(ctx context.Context, c *catalog.Catalog, discover Discover) (map[string][]string, error) {
	added := map[string][]string{}
	for _, name := range c.Names() {
		p := c.Plugins[name]
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

// Upstream discovers versions from where each kind's versions live:
// the module proxy for go, the npm registry for node, the GitHub
// releases of the repository for release and bazel. A GitHub token
// in GITHUB_TOKEN, where set, authenticates the releases listing.
func Upstream(ctx context.Context, p *catalog.Plugin) ([]string, error) {
	switch p.Kind {
	case catalog.KindGo:
		return goVersions(ctx, p.Module)
	case catalog.KindNode:
		return npmVersions(ctx, p.Package)
	case catalog.KindRelease, catalog.KindBazel:
		return releaseVersions(ctx, p.Repository, p.Tag)
	}
	return nil, fmt.Errorf("unknown kind %q", p.Kind)
}

func get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return body, nil
}

func goVersions(ctx context.Context, mod string) ([]string, error) {
	escaped, err := module.EscapePath(mod)
	if err != nil {
		return nil, err
	}
	body, err := get(ctx, "https://proxy.golang.org/"+escaped+"/@v/list", nil)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(body)), nil
}

func npmVersions(ctx context.Context, pkg string) ([]string, error) {
	body, err := get(ctx, "https://registry.npmjs.org/"+pkg, map[string]string{"Accept": "application/vnd.npm.install-v1+json"})
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
	header := map[string]string{"Accept": "application/vnd.github+json"}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		header["Authorization"] = "Bearer " + tok
	}
	var vs []string
	for page := 1; page <= 10; page++ {
		body, err := get(ctx, fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", repo, page), header)
		if err != nil {
			return nil, err
		}
		var releases []struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, err
		}
		for _, r := range releases {
			if r.Draft || r.Prerelease {
				continue
			}
			if v, ok := TagVersion(r.Tag, prefix, suffix); ok {
				vs = append(vs, v)
			}
		}
		if len(releases) < 100 {
			break
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
