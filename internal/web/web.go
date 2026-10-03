// Package web is the one HTTP read the catalog's tools make: a GET
// with headers, the body on 200, an error naming the URL otherwise.
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Get fetches the URL with the headers and returns the body.
func Get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	body, err := Open(ctx, url, header)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// Open fetches the URL with the headers and returns the body as a
// stream, open on 200 alone.
func Open(ctx context.Context, url string, header map[string]string) (io.ReadCloser, error) {
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
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp.Body, nil
}
