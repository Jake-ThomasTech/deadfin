// SPDX-License-Identifier: GPL-2.0-only

// Package fetch is the one HTTP path from Live TV to a provider: the M3U
// playlist of a tuner and the XMLTV file of a guide. It keeps each source
// polite (docs/spikes/livetv.md §4.3):
//
//   - A 401 marks the source "credentials rejected". No request goes out
//     until Reset, which an administrator triggers by editing or retrying
//     the source.
//   - A 403 or 429 backs off exponentially with jitter, from one minute to
//     one hour, and honors Retry-After. Several 403s in a row mark the source
//     "blocked by provider" until Reset.
//   - Every request carries the application User-Agent or the source
//     override.
//
// Upstream sends each request once and has no per-source state
// (M3uParser.cs:48-70, XmlTvListingsProvider.cs:59-115); the backoff and the
// stop on 401 are divergences.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/livetv/secret"
)

const (
	// MinBackoff is the first wait after a 403 or 429.
	MinBackoff = time.Minute
	// MaxBackoff caps the wait.
	MaxBackoff = time.Hour
	// BlockedAfter is the number of 403 responses in a row that marks a
	// source as blocked.
	BlockedAfter = 3
)

// ErrCredentialsRejected reports a source that answered 401. It stays
// rejected until Reset.
var ErrCredentialsRejected = errors.New("credentials rejected")

// ErrBlocked reports a source that answered 403 BlockedAfter times in a row.
// It stays blocked until Reset.
var ErrBlocked = errors.New("blocked by provider")

// ErrBackingOff reports a request refused locally because the source is in
// backoff. errors.As with *BackoffError gives the time of the next attempt.
var ErrBackingOff = errors.New("backing off")

// BackoffError is returned while a source is in backoff.
type BackoffError struct {
	Until time.Time
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("%s until %s", ErrBackingOff, e.Until.UTC().Format(time.RFC3339))
}

func (e *BackoffError) Is(target error) bool { return target == ErrBackingOff }

// StatusError is a provider response that is not 2xx.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("provider answered %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

// State is the politeness state of one source.
type State string

const (
	StateOK                  State = "ok"
	StateBackingOff          State = "backing-off"
	StateCredentialsRejected State = "credentials-rejected"
	StateBlocked             State = "blocked"
)

// Options configures a Client.
type Options struct {
	// HTTP is the client for every request. Required.
	HTTP *http.Client
	// Clock drives backoff. Required.
	Clock clock.Clock
	// UserAgent is the application User-Agent, for example
	// "Deadfin/0.4.0 (+https://github.com/deadfin-org/deadfin)". Required.
	UserAgent string
	// Jitter returns a random duration in [0, limit). Nil uses math/rand.
	Jitter func(limit time.Duration) time.Duration
}

// Client fetches provider sources. It is safe for concurrent use. A source
// is named by a key that the caller derives from the configured source (its
// id and URL), so an edited source starts with a clean state.
type Client struct {
	http      *http.Client
	clock     clock.Clock
	userAgent string
	jitter    func(time.Duration) time.Duration

	mu      sync.Mutex
	sources map[string]*sourceState
}

type sourceState struct {
	state     State
	until     time.Time
	backoff   time.Duration
	forbidden int
}

// New returns a Client.
func New(options Options) (*Client, error) {
	if options.HTTP == nil || options.Clock == nil || options.UserAgent == "" {
		return nil, errors.New("fetch: HTTP client, clock and user agent are required")
	}
	jitter := options.Jitter
	if jitter == nil {
		jitter = func(limit time.Duration) time.Duration {
			if limit <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(limit))) // #nosec G404 -- backoff jitter, not a secret.
		}
	}
	return &Client{
		http:      options.HTTP,
		clock:     options.Clock,
		userAgent: options.UserAgent,
		jitter:    jitter,
		sources:   make(map[string]*sourceState),
	}, nil
}

// Request names one fetch.
type Request struct {
	// Key identifies the source for politeness state.
	Key string
	// URL is the source URL. It may carry credentials.
	URL string
	// UserAgent overrides the application User-Agent when set.
	UserAgent string
	// Secrets masks the URL credentials in returned errors. The URL's own
	// credentials are always masked.
	Secrets secret.Set
}

// Get sends one GET for the request. On success the caller must close the
// body. Every returned error has its source credentials masked.
// UserAgent returns the application User-Agent.
func (c *Client) UserAgent() string { return c.userAgent }

func (c *Client) Get(ctx context.Context, request Request) (*http.Response, error) {
	secrets := secret.Collect([]string{request.URL})
	mask := func(err error) error { return request.Secrets.Err(secrets.Err(err)) }

	if err := c.admit(request.Key); err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return nil, mask(fmt.Errorf("fetch: build request: %w", err))
	}
	userAgent := c.userAgent
	if request.UserAgent != "" {
		userAgent = request.UserAgent
	}
	httpRequest.Header.Set("User-Agent", userAgent)

	response, err := c.http.Do(httpRequest)
	if err != nil {
		return nil, mask(fmt.Errorf("fetch: %w", err))
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		c.succeeded(request.Key)
		return response, nil
	}
	_ = response.Body.Close()
	return nil, c.failed(request.Key, response)
}

// Reset clears the state of a source, after an administrator edits or
// retries it.
func (c *Client) Reset(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sources, key)
}

// State reports the politeness state of a source.
func (c *Client) State(key string) State {
	c.mu.Lock()
	defer c.mu.Unlock()
	source, ok := c.sources[key]
	if !ok {
		return StateOK
	}
	if source.state == StateBackingOff && !c.clock.Now().Before(source.until) {
		return StateOK
	}
	return source.state
}

// Reject marks a source as credentials rejected, as a 401 on its own
// request does. A tuner uses it when a 401 on one of its streams must stop
// its playlist too.
func (c *Client) Reject(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.sources[key]
	if source == nil {
		source = &sourceState{}
		c.sources[key] = source
	}
	source.state = StateCredentialsRejected
}

func (c *Client) admit(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	source, ok := c.sources[key]
	if !ok {
		return nil
	}
	switch source.state {
	case StateCredentialsRejected:
		return fmt.Errorf("fetch: %w", ErrCredentialsRejected)
	case StateBlocked:
		return fmt.Errorf("fetch: %w", ErrBlocked)
	case StateBackingOff:
		if c.clock.Now().Before(source.until) {
			return fmt.Errorf("fetch: %w", &BackoffError{Until: source.until})
		}
	}
	return nil
}

func (c *Client) succeeded(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.sources, key)
}

func (c *Client) failed(key string, response *http.Response) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	source := c.sources[key]
	if source == nil {
		source = &sourceState{state: StateOK}
		c.sources[key] = source
	}
	status := &StatusError{StatusCode: response.StatusCode}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		source.state = StateCredentialsRejected
		return fmt.Errorf("fetch: %w: %w", ErrCredentialsRejected, status)
	case http.StatusForbidden, http.StatusTooManyRequests:
		if response.StatusCode == http.StatusForbidden {
			source.forbidden++
			if source.forbidden >= BlockedAfter {
				source.state = StateBlocked
				return fmt.Errorf("fetch: %w: %w", ErrBlocked, status)
			}
		} else {
			source.forbidden = 0
		}
		source.backoff = nextBackoff(source.backoff)
		wait := source.backoff + c.jitter(source.backoff/2)
		if retryAfter, ok := parseRetryAfter(response.Header.Get("Retry-After"), c.clock.Now()); ok && retryAfter > wait {
			wait = retryAfter
		}
		wait = min(wait, MaxBackoff)
		source.state = StateBackingOff
		source.until = c.clock.Now().Add(wait)
		return fmt.Errorf("fetch: %w: %w", status, &BackoffError{Until: source.until})
	default:
		source.forbidden = 0
		return fmt.Errorf("fetch: %w", status)
	}
}

func nextBackoff(previous time.Duration) time.Duration {
	if previous <= 0 {
		return MinBackoff
	}
	return min(previous*2, MaxBackoff)
}

// parseRetryAfter reads delay-seconds or an HTTP date (RFC 9110 §10.2.3).
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := at.Sub(now); wait > 0 {
			return wait, true
		}
	}
	return 0, false
}
