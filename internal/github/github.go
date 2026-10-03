// Package github reads what the catalog needs of a GitHub
// repository: its releases, for a kind whose versions are the
// repository's; a tag's commit and a file at it, for a go recipe
// built from a tag the module proxy does not list. A token in
// GITHUB_TOKEN, where set, authenticates every request.
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/greatliontech/pb-plugins/internal/web"
)

// API is the API's base URL; a test points it at a fake.
var API = "https://api.github.com"

// Release is one release of a repository: its tag, and whether it
// is a draft or a prerelease.
type Release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Releases lists the repository's releases, newest first, every
// page up to the tenth of a hundred.
func Releases(ctx context.Context, repo string) ([]Release, error) {
	var all []Release
	for page := 1; page <= 10; page++ {
		body, err := get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", API, repo, page))
		if err != nil {
			return nil, err
		}
		var releases []Release
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, err
		}
		all = append(all, releases...)
		if len(releases) < 100 {
			break
		}
	}
	return all, nil
}

// TagCommit is the commit a tag of the repository names: the tag's
// own object where the tag is lightweight, the tagged commit where it
// is annotated.
func TagCommit(ctx context.Context, repo, tag string) (string, error) {
	body, err := get(ctx, fmt.Sprintf("%s/repos/%s/git/ref/tags/%s", API, repo, escapePath(tag)))
	if err != nil {
		return "", err
	}
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return "", err
	}
	if ref.Object.Type != "tag" {
		return ref.Object.SHA, nil
	}
	body, err = get(ctx, fmt.Sprintf("%s/repos/%s/git/tags/%s", API, repo, ref.Object.SHA))
	if err != nil {
		return "", err
	}
	var tagObject struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &tagObject); err != nil {
		return "", err
	}
	return tagObject.Object.SHA, nil
}

// File is the bytes of a file of the repository at a ref.
func File(ctx context.Context, repo, ref, path string) ([]byte, error) {
	body, err := get(ctx, fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", API, repo, escapePath(path), url.QueryEscape(ref)))
	if err != nil {
		return nil, err
	}
	var content struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(body, &content); err != nil {
		return nil, err
	}
	if content.Encoding != "base64" {
		return nil, fmt.Errorf("%s at %s: content encoded %q, not base64", path, ref, content.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
}

// escapePath escapes each segment of a path for a URL, the slashes
// kept.
func escapePath(p string) string {
	segments := strings.Split(p, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

func get(ctx context.Context, u string) ([]byte, error) {
	header := map[string]string{"Accept": "application/vnd.github+json"}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		header["Authorization"] = "Bearer " + tok
	}
	return web.Get(ctx, u, header)
}
