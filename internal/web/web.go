// Package web is the one HTTP read the catalog's tools make: a GET
// with headers, the body on 200, an error naming the URL otherwise.
// An answer that refuses the request for the moment — 429, or a 5xx
// — is retried a bounded number of times, the wait the server's
// `Retry-After` where it names one within a minute (a longer ask is
// final), else doubling from a second: upstreams throttle the
// runners, and a build or publish is not failed by a refusal that
// a later request answers. Any other status, and a failure to get
// an answer at all, is final at once.
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// retries is the number of times a refused request is repeated.
const retries = 5

// retryWait is the first wait before a repeated request where the
// server names none, doubling with each repetition.
var retryWait = time.Second

// retryAfterMax bounds the wait the server may name; past it the
// refusal is final.
const retryAfterMax = time.Minute

// sleep waits d, or until the context ends; the tests replace it to
// read the waits.
var sleep = func(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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
	backoff := retryWait
	for attempt := 0; ; attempt++ {
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
		if resp.StatusCode == http.StatusOK {
			return resp.Body, nil
		}
		resp.Body.Close()
		if !transient(resp.StatusCode) {
			return nil, fmt.Errorf("%s: %s", url, resp.Status)
		}
		if attempt == retries {
			return nil, fmt.Errorf("%s: %s, after %d repetitions", url, resp.Status, retries)
		}
		wait, named, err := retryAfter(resp.Header.Get("Retry-After"))
		if err != nil {
			return nil, fmt.Errorf("%s: %s, %w", url, resp.Status, err)
		}
		if !named {
			wait, backoff = backoff, backoff*2
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
	}
}

// transient reports whether a status refuses the request for the
// moment rather than finally.
func transient(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryAfter reads a Retry-After header: a number of seconds (none
// below zero) or an HTTP date, named when present and readable; an
// ask past retryAfterMax is an error, the refusal final. The seconds
// are bounded before they become a duration, which a large count
// would overflow.
func retryAfter(h string) (wait time.Duration, named bool, err error) {
	if h == "" {
		return 0, false, nil
	}
	if secs, err := strconv.Atoi(h); err == nil {
		if secs < 0 {
			return 0, false, nil
		}
		if secs > int(retryAfterMax/time.Second) {
			return 0, false, fmt.Errorf("asked to retry after %ds", secs)
		}
		return time.Duration(secs) * time.Second, true, nil
	}
	at, err := http.ParseTime(h)
	if err != nil {
		return 0, false, nil
	}
	wait = time.Until(at)
	if wait < 0 {
		wait = 0
	}
	if wait > retryAfterMax {
		return 0, false, fmt.Errorf("asked to retry after %s", wait.Round(time.Second))
	}
	return wait, true, nil
}
