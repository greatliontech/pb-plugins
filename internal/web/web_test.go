package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A refusal for the moment is repeated until answered, a final one
// not at all, and one that outlasts the bound names it.
func TestOpenRepeatsTransientRefusals(t *testing.T) {
	retryWait = time.Millisecond
	defer func() { retryWait = time.Second }()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch r.URL.Path {
		case "/throttled":
			if n < 3 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, "answered")
		case "/down":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/missing":
			http.NotFound(w, r)
		case "/later":
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/far":
			w.Header().Set("Retry-After", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		case "/overflow":
			w.Header().Set("Retry-After", "9223372037")
			w.WriteHeader(http.StatusTooManyRequests)
		case "/negative":
			if n < 2 {
				w.Header().Set("Retry-After", "-5")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, "negative")
		case "/slow":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	if b, err := Get(context.Background(), srv.URL+"/throttled", nil); err != nil || string(b) != "answered" {
		t.Errorf("a throttled request: %q %v", b, err)
	}
	if n := hits.Load(); n != 3 {
		t.Errorf("a throttled request made %d requests, not 3", n)
	}
	hits.Store(0)
	if _, err := Get(context.Background(), srv.URL+"/down", nil); err == nil || !strings.Contains(err.Error(), "503 Service Unavailable, after 5 repetitions") {
		t.Errorf("a server down: %v", err)
	}
	if n := hits.Load(); n != 6 {
		t.Errorf("a server down took %d requests, not 6", n)
	}
	hits.Store(0)
	if _, err := Get(context.Background(), srv.URL+"/missing", nil); err == nil || !strings.HasSuffix(err.Error(), "404 Not Found") {
		t.Errorf("a missing asset: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("a missing asset took %d requests, not 1", n)
	}
	// An ask past the bound, as seconds, as a date (two hours ahead,
	// rounded to the second either way), or as a count of seconds no
	// duration holds, is final on the first answer.
	bounded, cancelBounded := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelBounded()
	for path, want := range map[string]string{"/later": "asked to retry after 3600s", "/far": "asked to retry after ", "/overflow": "asked to retry after 9223372037s"} {
		hits.Store(0)
		if _, err := Get(bounded, srv.URL+path, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: an ask past the bound: %v", path, err)
		}
		if n := hits.Load(); n != 1 {
			t.Errorf("%s: an ask past the bound took %d requests, not 1", path, n)
		}
	}
	// A count below zero names nothing: the wait is the backoff's.
	hits.Store(0)
	if b, err := Get(context.Background(), srv.URL+"/negative", nil); err != nil || string(b) != "negative" {
		t.Errorf("a negative ask: %q %v", b, err)
	}
	// A wait the context ends first is left, the URL named.
	hits.Store(0)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Get(ctx, srv.URL+"/slow", nil); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "/slow") {
		t.Errorf("a wait outlasting the context: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the wait outlasted the context")
	}
}

// A date ahead is waited for until it comes: three seconds ahead,
// floored to the second by the date's precision, the wait read from
// the sleep.
func TestOpenWaitsForADate(t *testing.T) {
	var waits []time.Duration
	saved := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	defer func() { sleep = saved }()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 2 {
			w.Header().Set("Retry-After", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, "dated")
	}))
	defer srv.Close()
	if b, err := Get(context.Background(), srv.URL, nil); err != nil || string(b) != "dated" {
		t.Errorf("a dated ask: %q %v", b, err)
	}
	if len(waits) != 1 || waits[0] <= time.Second || waits[0] > 3*time.Second {
		t.Errorf("the wait for a date three seconds ahead: %v", waits)
	}
}

// What a Retry-After names, read from the sleep: a count of seconds
// within the minute waited for as named, the minute itself included,
// one past it final, a count below zero and an unreadable value
// naming nothing, the backoff's wait taken instead.
func TestOpenAsks(t *testing.T) {
	var waits []time.Duration
	saved := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	defer func() { sleep = saved }()
	var ask atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", ask.Load().(string))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	for _, tc := range []struct {
		ask   string
		waits []time.Duration
		final bool
	}{
		{"60", []time.Duration{time.Minute}, false},
		{"61", nil, true},
		{"0", []time.Duration{0}, false},
		{"-5", []time.Duration{time.Second}, false},
		{"soon", []time.Duration{time.Second}, false},
	} {
		waits = nil
		hits.Store(0)
		ask.Store(tc.ask)
		_, err := Get(context.Background(), srv.URL, nil)
		switch {
		case tc.final && (err == nil || !strings.Contains(err.Error(), "asked to retry after 61s") || hits.Load() != 1):
			t.Errorf("Retry-After %q: %v after %d requests", tc.ask, err, hits.Load())
		case !tc.final && err != nil:
			t.Errorf("Retry-After %q: %v", tc.ask, err)
		case fmt.Sprint(waits) != fmt.Sprint(tc.waits):
			t.Errorf("Retry-After %q: waited %v, wanted %v", tc.ask, waits, tc.waits)
		}
	}
}

// The waits double from the first where the server names none, and
// follow the server where it does, the doubling untouched by an
// ask between: read from the sleeps rather than the clock.
func TestOpenWaits(t *testing.T) {
	var waits []time.Duration
	saved := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	defer func() { sleep = saved }()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch hits {
		case 1, 2, 4:
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, "ok")
		}
	}))
	defer srv.Close()
	if _, err := Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 7 * time.Second, 4 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Errorf("the waits: %v, wanted %v", waits, want)
	}
}
