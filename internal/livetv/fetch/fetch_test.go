// SPDX-License-Identifier: GPL-2.0-only

package fetch_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
)

const (
	appUserAgent = "Deadfin/test (+https://github.com/deadfin-org/deadfin)"
	marker       = "MARKERpw-7b1e"
)

type provider struct {
	server     *httptest.Server
	requests   atomic.Int32
	status     atomic.Int32
	retryAfter atomic.Value
	userAgents chan string
}

func newProvider(t *testing.T) *provider {
	t.Helper()
	p := &provider{userAgents: make(chan string, 16)}
	p.status.Store(http.StatusOK)
	p.retryAfter.Store("")
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		p.userAgents <- r.UserAgent()
		if value := p.retryAfter.Load().(string); value != "" {
			w.Header().Set("Retry-After", value)
		}
		w.WriteHeader(int(p.status.Load()))
		_, _ = w.Write([]byte("#EXTM3U\n"))
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *provider) url() string {
	return p.server.URL + "/get.php?username=alice&password=" + marker + "&type=m3u_plus"
}

func newClient(t *testing.T, fake *clock.Fake) *fetch.Client {
	t.Helper()
	client, err := fetch.New(fetch.Options{
		HTTP:      &http.Client{},
		Clock:     fake,
		UserAgent: appUserAgent,
		Jitter:    func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func get(t *testing.T, client *fetch.Client, p *provider, userAgent string) error {
	t.Helper()
	response, err := client.Get(t.Context(), fetch.Request{Key: "tuner-1", URL: p.url(), UserAgent: userAgent})
	if err == nil {
		_ = response.Body.Close()
	}
	if err != nil && strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaks the password: %v", err)
	}
	return err
}

func TestGetSendsApplicationUserAgentOrOverride(t *testing.T) {
	p := newProvider(t)
	client := newClient(t, clock.NewFake(time.Unix(0, 0)))
	if err := get(t, client, p, ""); err != nil {
		t.Fatal(err)
	}
	if got := <-p.userAgents; got != appUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, appUserAgent)
	}
	if err := get(t, client, p, "VLC/3.0.20"); err != nil {
		t.Fatal(err)
	}
	if got := <-p.userAgents; got != "VLC/3.0.20" {
		t.Fatalf("User-Agent = %q, want the override", got)
	}
}

// L16 a: a wrong password reaches "credentials rejected" after one request,
// and no further request leaves until Reset.
func TestUnauthorizedStopsUntilReset(t *testing.T) {
	p := newProvider(t)
	fake := clock.NewFake(time.Unix(0, 0))
	client := newClient(t, fake)
	p.status.Store(http.StatusUnauthorized)

	if err := get(t, client, p, ""); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("first Get = %v, want ErrCredentialsRejected", err)
	}
	fake.Advance(24 * time.Hour)
	for range 5 {
		if err := get(t, client, p, ""); !errors.Is(err, fetch.ErrCredentialsRejected) {
			t.Fatalf("Get = %v, want ErrCredentialsRejected", err)
		}
	}
	if got := p.requests.Load(); got != 1 {
		t.Fatalf("provider saw %d requests, want 1", got)
	}
	if got := client.State("tuner-1"); got != fetch.StateCredentialsRejected {
		t.Fatalf("State = %q", got)
	}

	client.Reset("tuner-1")
	p.status.Store(http.StatusOK)
	if err := get(t, client, p, ""); err != nil {
		t.Fatalf("Get after Reset = %v", err)
	}
}

// L16 d: a 403 storm backs off exponentially and then stops as blocked.
func TestForbiddenBacksOffThenBlocks(t *testing.T) {
	p := newProvider(t)
	fake := clock.NewFake(time.Unix(0, 0))
	client := newClient(t, fake)
	p.status.Store(http.StatusForbidden)

	err := get(t, client, p, "")
	var backoff *fetch.BackoffError
	if !errors.As(err, &backoff) || !backoff.Until.Equal(fake.Now().Add(fetch.MinBackoff)) {
		t.Fatalf("first 403: %v, want backoff of %s", err, fetch.MinBackoff)
	}
	// Inside the window nothing is sent.
	if err := get(t, client, p, ""); !errors.Is(err, fetch.ErrBackingOff) {
		t.Fatalf("Get in backoff = %v", err)
	}
	if got := p.requests.Load(); got != 1 {
		t.Fatalf("provider saw %d requests in backoff, want 1", got)
	}

	fake.Advance(fetch.MinBackoff)
	err = get(t, client, p, "")
	if !errors.As(err, &backoff) || !backoff.Until.Equal(fake.Now().Add(2*fetch.MinBackoff)) {
		t.Fatalf("second 403: %v, want doubled backoff", err)
	}

	fake.Advance(2 * fetch.MinBackoff)
	if err := get(t, client, p, ""); !errors.Is(err, fetch.ErrBlocked) {
		t.Fatalf("third 403 = %v, want ErrBlocked", err)
	}
	fake.Advance(24 * time.Hour)
	if err := get(t, client, p, ""); !errors.Is(err, fetch.ErrBlocked) {
		t.Fatalf("Get when blocked = %v", err)
	}
	if got := p.requests.Load(); got != fetch.BlockedAfter {
		t.Fatalf("provider saw %d requests, want %d", got, fetch.BlockedAfter)
	}
}

func TestTooManyRequestsHonorsRetryAfter(t *testing.T) {
	p := newProvider(t)
	fake := clock.NewFake(time.Unix(0, 0))
	client := newClient(t, fake)
	p.status.Store(http.StatusTooManyRequests)
	p.retryAfter.Store("600")

	err := get(t, client, p, "")
	var backoff *fetch.BackoffError
	if !errors.As(err, &backoff) || !backoff.Until.Equal(fake.Now().Add(10*time.Minute)) {
		t.Fatalf("429: %v, want Retry-After of 10m", err)
	}
	p.status.Store(http.StatusOK)
	fake.Advance(10 * time.Minute)
	if err := get(t, client, p, ""); err != nil {
		t.Fatalf("Get after Retry-After = %v", err)
	}
	if got := client.State("tuner-1"); got != fetch.StateOK {
		t.Fatalf("State after success = %q", got)
	}
}

func TestBackoffIsCappedAtMaxBackoff(t *testing.T) {
	p := newProvider(t)
	fake := clock.NewFake(time.Unix(0, 0))
	client := newClient(t, fake)
	p.status.Store(http.StatusTooManyRequests)
	p.retryAfter.Store("86400")
	err := get(t, client, p, "")
	var backoff *fetch.BackoffError
	if !errors.As(err, &backoff) || !backoff.Until.Equal(fake.Now().Add(fetch.MaxBackoff)) {
		t.Fatalf("429 with a long Retry-After: %v, want the %s cap", err, fetch.MaxBackoff)
	}
}

func TestOtherStatusIsAPlainError(t *testing.T) {
	p := newProvider(t)
	client := newClient(t, clock.NewFake(time.Unix(0, 0)))
	p.status.Store(http.StatusNotFound)
	err := get(t, client, p, "")
	var status *fetch.StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
		t.Fatalf("Get = %v, want StatusError 404", err)
	}
	if got := client.State("tuner-1"); got != fetch.StateOK {
		t.Fatalf("State = %q, want ok", got)
	}
}

func TestTransportErrorsAreRedacted(t *testing.T) {
	client := newClient(t, clock.NewFake(time.Unix(0, 0)))
	_, err := client.Get(t.Context(), fetch.Request{
		Key: "tuner-1",
		URL: "http://127.0.0.1:1/live/alice/" + marker + "/1.ts",
	})
	if err == nil {
		t.Fatal("Get to a closed port succeeded")
	}
	if strings.Contains(err.Error(), marker) {
		t.Fatalf("error leaks the password: %v", err)
	}
}
