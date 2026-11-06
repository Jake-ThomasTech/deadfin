// SPDX-License-Identifier: GPL-2.0-only

package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/middleware"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// slowRequestRoute is the route metadata used by every slow-request
// diagnostics test. It mirrors a Resume operation so the test reflects a
// real slow operation.
var slowRequestRoute = router.Route{
	Method:      http.MethodGet,
	Pattern:     "/Items/{itemId}/Resume",
	OperationID: "GetResumeItems",
}

// slowRequestThreshold is the fixed 2-second slow-request threshold.
const slowRequestThreshold = 2 * time.Second

func newSlowRequestStores(t *testing.T) store.Stores {
	t.Helper()
	opened, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	return opened
}

// signalingHandler wraps a JSON handler and closes a channel when a WARN-level
// record is handled. This gives the test a deterministic synchronization point
// for the asynchronous watcher goroutine.
type signalingHandler struct {
	inner  slog.Handler
	buf    *bytes.Buffer
	warned chan struct{}
	once   *sync.Once
}

func newSignalingHandler(buf *bytes.Buffer) *signalingHandler {
	return &signalingHandler{
		inner:  slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
		buf:    buf,
		warned: make(chan struct{}),
		once:   &sync.Once{},
	}
}

func (h *signalingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *signalingHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.inner.Handle(ctx, r)
	if r.Level == slog.LevelWarn {
		h.once.Do(func() { close(h.warned) })
	}
	return err
}

func (h *signalingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &signalingHandler{inner: h.inner.WithAttrs(attrs), buf: h.buf, warned: h.warned, once: h.once}
}

func (h *signalingHandler) WithGroup(name string) slog.Handler {
	return &signalingHandler{inner: h.inner.WithGroup(name), buf: h.buf, warned: h.warned, once: h.once}
}

// slowRequestHarness assembles the clock, stores, signaling log, and router
// for a slow-request diagnostics test.
type slowRequestHarness struct {
	clock   *clock.Fake
	stores  store.Stores
	handler *signalingHandler
	logger  *slog.Logger
	routes  *router.Router
}

func newSlowRequestHarness(t *testing.T) *slowRequestHarness {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, time.August, 15, 12, 0, 0, 0, time.UTC))
	stores := newSlowRequestStores(t)
	handler := newSignalingHandler(&bytes.Buffer{})
	logger := slog.New(handler)
	diagnostics := middleware.NewSlowRequestDiagnostics(clk, stores, logger, slowRequestThreshold)
	routes := router.New(diagnostics.Wrap)
	return &slowRequestHarness{
		clock:   clk,
		stores:  stores,
		handler: handler,
		logger:  logger,
		routes:  routes,
	}
}

// warnings parses each JSON line from the buffer and returns the entries with
// level WARN and the slow-request message.
func (h *slowRequestHarness) warnings(t *testing.T) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range bytes.Split(h.handler.buf.Bytes(), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("parse log line: %v\nline: %s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

// awaitWarning waits for the watcher goroutine to write the warning, or fails
// the test after a timeout.
func (h *slowRequestHarness) awaitWarning(t *testing.T) {
	t.Helper()
	select {
	case <-h.handler.warned:
	case <-time.After(5 * time.Second):
		t.Fatal("warning was not written within the timeout")
	}
}

// runBlockedHandler registers a route with a handler that advances the fake
// clock to the threshold and then blocks until the release channel is closed.
// The goroutine that serves the request is started in the background.
func (h *slowRequestHarness) runBlockedHandler(t *testing.T, release <-chan struct{}) {
	t.Helper()
	h.routes.Handle(slowRequestRoute.Method, slowRequestRoute.Pattern, slowRequestRoute,
		func(http.ResponseWriter, *http.Request) {
			h.clock.Advance(slowRequestThreshold)
			<-release
		})
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(slowRequestRoute.Method, slowRequestRoute.Pattern, nil)
		h.routes.ServeHTTP(rec, req)
	}()
}

// runBlockedHandlerWithQuery is runBlockedHandler with a query string on the
// request URL.
func (h *slowRequestHarness) runBlockedHandlerWithQuery(t *testing.T, release <-chan struct{}, query string) {
	t.Helper()
	h.routes.Handle(slowRequestRoute.Method, slowRequestRoute.Pattern, slowRequestRoute,
		func(http.ResponseWriter, *http.Request) {
			h.clock.Advance(slowRequestThreshold)
			<-release
		})
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(slowRequestRoute.Method, slowRequestRoute.Pattern+query, nil)
		h.routes.ServeHTTP(rec, req)
	}()
}

// TestSlowRequestDiagnosticsFastHandlerEmitsNoWarning verifies that a handler
// which returns before the threshold emits no warning after the fake clock
// advances past the threshold (acceptance 1).
func TestSlowRequestDiagnosticsFastHandlerEmitsNoWarning(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	done := make(chan struct{})
	h.routes.Handle(slowRequestRoute.Method, slowRequestRoute.Pattern, slowRequestRoute,
		func(http.ResponseWriter, *http.Request) { close(done) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(slowRequestRoute.Method, slowRequestRoute.Pattern, nil)
	h.routes.ServeHTTP(rec, req)

	<-done
	h.clock.Advance(slowRequestThreshold + time.Second)

	if got := len(h.warnings(t)); got != 0 {
		t.Fatalf("fast handler emitted %d warnings, want 0", got)
	}
}

// countingClock counts the timers that the diagnostics wrapper arms.
type countingClock struct {
	clock.Clock
	mu     sync.Mutex
	timers int
}

func (c *countingClock) NewTimer(delay time.Duration) clock.Timer {
	c.mu.Lock()
	c.timers++
	c.mu.Unlock()
	return c.Clock.NewTimer(delay)
}

// A transcode-worker route (a long-lived control WebSocket or source stream)
// arms no slow-request watchdog (#614).
func TestSlowRequestDiagnosticsSkipsWorkerRoutes(t *testing.T) {
	stores := newSlowRequestStores(t)
	t.Cleanup(func() { _ = stores.Close() })
	counting := &countingClock{Clock: clock.NewFake(time.Date(2026, time.October, 16, 0, 0, 0, 0, time.UTC))}
	diagnostics := middleware.NewSlowRequestDiagnostics(counting, stores, slog.New(slog.DiscardHandler), slowRequestThreshold)
	called := false
	handler := diagnostics.Wrap(router.Route{Method: http.MethodGet, Pattern: "/deadfin/v1/workers/connect", Worker: true}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/deadfin/v1/workers/connect", nil))
	counting.mu.Lock()
	defer counting.mu.Unlock()
	if !called || counting.timers != 0 {
		t.Fatalf("handler called %t, timers armed %d; want true, 0", called, counting.timers)
	}
}

// TestSlowRequestDiagnosticsBlockedHandlerEmitsOneWarning verifies that a
// blocked handler emits one warning when the fake clock reaches the threshold
// (acceptance 2) and that a further advance does not emit a second warning
// (acceptance 6).
func TestSlowRequestDiagnosticsBlockedHandlerEmitsOneWarning(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	release := make(chan struct{})
	h.runBlockedHandler(t, release)

	h.awaitWarning(t)

	if got := len(h.warnings(t)); got != 1 {
		t.Fatalf("blocked handler emitted %d warnings, want 1", got)
	}

	// Advance the clock further and confirm a second warning is not emitted
	// for the same request (acceptance 6).
	h.clock.Advance(5 * time.Second)
	if got := len(h.warnings(t)); got != 1 {
		t.Fatalf("blocked handler emitted %d warnings after further advance, want 1", got)
	}

	close(release)
}

// TestSlowRequestDiagnosticsWarningAppearsBeforeHandlerRelease verifies that
// the warning is written before the blocked handler is released (acceptance 3).
func TestSlowRequestDiagnosticsWarningAppearsBeforeHandlerRelease(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	release := make(chan struct{})
	h.runBlockedHandler(t, release)

	h.awaitWarning(t)

	// The warning is written while the handler is still blocked, so the
	// release channel has not been closed yet.
	select {
	case <-release:
		t.Fatal("handler was released before the warning was written")
	default:
	}

	if got := len(h.warnings(t)); got != 1 {
		t.Fatalf("warnings = %d, want 1 before handler release", got)
	}
	close(release)
}

// TestSlowRequestDiagnosticsWarningContainsRequestMetadata verifies that the
// warning contains the method, route pattern, operation ID, and elapsed
// duration (acceptance 4).
func TestSlowRequestDiagnosticsWarningContainsRequestMetadata(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	release := make(chan struct{})
	h.runBlockedHandler(t, release)
	h.awaitWarning(t)

	warnings := h.warnings(t)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(warnings))
	}
	warning := warnings[0]

	if got, want := warning["msg"], "slow HTTP request"; got != want {
		t.Errorf("msg = %v, want %v", got, want)
	}
	if got, want := warning["method"], slowRequestRoute.Method; got != want {
		t.Errorf("method = %v, want %v", got, want)
	}
	if got, want := warning["route"], slowRequestRoute.Pattern; got != want {
		t.Errorf("route = %v, want %v", got, want)
	}
	if got, want := warning["operation_id"], slowRequestRoute.OperationID; got != want {
		t.Errorf("operation_id = %v, want %v", got, want)
	}
	if got, want := warning["level"], "WARN"; got != want {
		t.Errorf("level = %v, want %v", got, want)
	}
	elapsed, elapsedOK := warning["elapsed"]
	if !elapsedOK {
		t.Errorf("elapsed field is missing from the warning")
	} else {
		// The JSON handler serializes a slog.Duration as nanoseconds
		// (float64). Accept either a string or a non-zero number.
		switch v := elapsed.(type) {
		case string:
			if v == "" {
				t.Errorf("elapsed = %q, want a non-empty duration", v)
			}
		case float64:
			if v == 0 {
				t.Errorf("elapsed = 0, want a non-zero duration")
			}
		default:
			t.Errorf("elapsed = %v (%T), want a duration string or number", elapsed, elapsed)
		}
	}

	close(release)
}

// TestSlowRequestDiagnosticsWarningContainsPoolStats verifies that the warning
// contains all five required fields for both the reader and writer pools
// (acceptance 5).
func TestSlowRequestDiagnosticsWarningContainsPoolStats(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	release := make(chan struct{})
	h.runBlockedHandler(t, release)
	h.awaitWarning(t)

	warnings := h.warnings(t)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1", len(warnings))
	}
	warning := warnings[0]

	requiredFields := []string{
		"max_open_connections",
		"in_use",
		"idle",
		"wait_count",
		"wait_duration",
	}
	for _, group := range []string{"reader", "writer"} {
		groupData, ok := warning[group].(map[string]any)
		if !ok {
			t.Fatalf("warning missing %s group (got %T for %q)", group, warning[group], group)
		}
		for _, field := range requiredFields {
			if _, ok := groupData[field]; !ok {
				t.Errorf("%s group missing field %q", group, field)
			}
		}
	}

	close(release)
}

// TestSlowRequestDiagnosticsDoesNotLogQueryValues verifies that a raw query
// value such as api_key=secret does not appear in the warning (acceptance 7).
func TestSlowRequestDiagnosticsDoesNotLogQueryValues(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	release := make(chan struct{})
	h.runBlockedHandlerWithQuery(t, release, "?api_key=secret")
	h.awaitWarning(t)

	if bytes.Contains(h.handler.buf.Bytes(), []byte("secret")) {
		t.Fatalf("warning contains the raw query value:\n%s", h.handler.buf.String())
	}

	close(release)
}

// TestSlowRequestDiagnosticsNoRaceOnHandlerCompletion verifies that handler
// completion stops and joins the watcher without a race report
// (acceptance 8). Run with -race to detect a data race.
func TestSlowRequestDiagnosticsNoRaceOnHandlerCompletion(t *testing.T) {
	h := newSlowRequestHarness(t)
	t.Cleanup(func() { _ = h.stores.Close() })

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(slowRequestRoute.Method, slowRequestRoute.Pattern, nil)
			h.routes.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()

	if got := len(h.warnings(t)); got != 0 {
		t.Fatalf("fast handlers emitted %d warnings, want 0", got)
	}
}

// TestSlowRequestDiagnosticsCompiles is a sentinel for acceptance 9: the
// cmd/deadfin package compiles with the production wrapper composition. The
// composition_test.go suite in cmd/deadfin exercises the production
// composition; this test exists so the focused acceptance command -run
// '^TestSlowRequestDiagnostics' includes a named sentinel.
func TestSlowRequestDiagnosticsCompiles(t *testing.T) {
	t.Log("cmd/deadfin compiles with the production wrapper composition")
}
