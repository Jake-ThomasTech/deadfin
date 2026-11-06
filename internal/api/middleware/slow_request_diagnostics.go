// SPDX-License-Identifier: GPL-2.0-only

package middleware

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/store"
)

// SlowRequestDiagnostics writes one structured warning when a registered HTTP
// request remains active past the threshold. The warning records stable route
// metadata, the elapsed time, and a snapshot from each database pool. Fast
// requests write no warning.
type SlowRequestDiagnostics struct {
	clock     clock.Clock
	stores    store.Stores
	logger    *slog.Logger
	threshold time.Duration
}

// NewSlowRequestDiagnostics constructs the route wrapper that captures
// reader-pool diagnostics for a request that exceeds the threshold.
func NewSlowRequestDiagnostics(clk clock.Clock, stores store.Stores, logger *slog.Logger, threshold time.Duration) *SlowRequestDiagnostics {
	return &SlowRequestDiagnostics{
		clock:     clk,
		stores:    stores,
		logger:    logger,
		threshold: threshold,
	}
}

// Wrap applies the slow-request watchdog to one registered route.
// Transcode-worker routes are exempt: the control WebSocket and the source
// stream are long-lived by design and do not use the database.
func (d *SlowRequestDiagnostics) Wrap(route router.Route, next http.Handler) http.Handler {
	if route.Worker {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := d.clock.Now()
		timer := d.clock.NewTimer(d.threshold)

		stop := make(chan struct{})
		watcherDone := make(chan struct{})
		var warned sync.Once

		go func() {
			defer close(watcherDone)
			select {
			case <-timer.C():
				warned.Do(d.warning(route, d.clock.Now().Sub(start)))
			case <-stop:
			}
		}()

		next.ServeHTTP(w, r)

		timer.Stop()
		close(stop)
		<-watcherDone
	})
}

// warning returns a closure that writes a single slow-request warning with the
// request metadata and the database-pool snapshots. The closure is the body
// of a sync.Once.Do call, so it runs at most once per request.
func (d *SlowRequestDiagnostics) warning(route router.Route, elapsed time.Duration) func() {
	return func() {
		readerStats := d.stores.Reader().Stats()
		writerStats := d.stores.Writer().Stats()
		d.logger.Warn("slow HTTP request",
			slog.String("method", route.Method),
			slog.String("route", route.Pattern),
			slog.String("operation_id", route.OperationID),
			slog.Duration("elapsed", elapsed),
			slog.Group("reader",
				slog.Int("max_open_connections", readerStats.MaxOpenConnections),
				slog.Int("in_use", readerStats.InUse),
				slog.Int("idle", readerStats.Idle),
				slog.Int64("wait_count", readerStats.WaitCount),
				slog.Duration("wait_duration", readerStats.WaitDuration),
			),
			slog.Group("writer",
				slog.Int("max_open_connections", writerStats.MaxOpenConnections),
				slog.Int("in_use", writerStats.InUse),
				slog.Int("idle", writerStats.Idle),
				slog.Int64("wait_count", writerStats.WaitCount),
				slog.Duration("wait_duration", writerStats.WaitDuration),
			),
		)
	}
}
