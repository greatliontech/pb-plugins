package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Releases walks the pages; a tag's commit is the ref's object for a
// lightweight tag and the tag object's for an annotated one; a file
// at a ref is decoded from the contents API; a tag's odd characters
// are escaped; the token, where set, is sent.
func TestReleasesTagCommitAndFile(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "headers", http.StatusBadRequest)
			return
		}
		switch {
		case r.URL.Path == "/repos/o/r/releases" && r.URL.Query().Get("page") == "1":
			var items []string
			for i := 0; i < 100; i++ {
				items = append(items, fmt.Sprintf(`{"tag_name":"v1.%d.0","draft":false,"prerelease":false}`, i))
			}
			fmt.Fprint(w, "["+strings.Join(items, ",")+"]")
		case r.URL.Path == "/repos/o/r/releases" && r.URL.Query().Get("page") == "2":
			fmt.Fprint(w, `[{"tag_name":"v0.1.0","draft":true,"prerelease":false}]`)
		case r.URL.Path == "/repos/o/r/git/ref/tags/light":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"aaa"}}`)
		case r.URL.Path == "/repos/o/r/git/ref/tags/heavy":
			fmt.Fprint(w, `{"object":{"type":"tag","sha":"ttt"}}`)
		case r.URL.Path == "/repos/o/r/git/tags/ttt":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"bbb"}}`)
		case r.URL.EscapedPath() == "/repos/o/r/git/ref/tags/odd%23tag%3Fx/sub":
			fmt.Fprint(w, `{"object":{"type":"commit","sha":"ccc"}}`)
		case r.URL.Path == "/repos/o/r/contents/sub/go.mod" && r.URL.Query().Get("ref") == "aaa":
			fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte("module m/v5\n"))+"\n")
		case r.URL.Path == "/repos/o/r/contents/big" && r.URL.Query().Get("ref") == "aaa":
			// A file past the API's inline size comes with no content.
			fmt.Fprint(w, `{"encoding":"none","content":""}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	saved := API
	API = srv.URL
	defer func() { API = saved }()
	rs, err := Releases(context.Background(), "o/r")
	if err != nil || len(rs) != 101 || rs[0].Tag != "v1.0.0" || !rs[100].Draft {
		t.Fatalf("releases: %d %v", len(rs), err)
	}
	for tag, want := range map[string]string{"light": "aaa", "heavy": "bbb", "odd#tag?x/sub": "ccc"} {
		if sha, err := TagCommit(context.Background(), "o/r", tag); err != nil || sha != want {
			t.Fatalf("tag %q: %q %v", tag, sha, err)
		}
	}
	if _, err := TagCommit(context.Background(), "o/r", "none"); err == nil {
		t.Fatal("a missing tag resolved")
	}
	if b, err := File(context.Background(), "o/r", "aaa", "sub/go.mod"); err != nil || string(b) != "module m/v5\n" {
		t.Fatalf("the file at the ref: %q %v", b, err)
	}
	if _, err := File(context.Background(), "o/r", "aaa", "big"); err == nil || !strings.Contains(err.Error(), "not base64") {
		t.Fatalf("a file the API does not inline: %v", err)
	}
}
