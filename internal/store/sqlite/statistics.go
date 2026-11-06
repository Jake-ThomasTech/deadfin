// SPDX-License-Identifier: GPL-2.0-only

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

// DefaultStatisticsInterval is the period of the planner statistics refresh.
// SQLite recommends PRAGMA optimize every few hours on long-lived
// connections (https://sqlite.org/lang_analyze.html#periodically_run_pragma_optimize_).
const DefaultStatisticsInterval = 3 * time.Hour

// statisticsAnalysisLimit bounds the rows that PRAGMA optimize examines per
// index. SQLite recommends a value between 100 and 1000.
const statisticsAnalysisLimit = 400

// statisticsDriftFactor is the ratio between the stored and the real items
// row count above which a run does a full ANALYZE instead of PRAGMA optimize.
const statisticsDriftFactor = 2

// StatisticsOptions configures the planner statistics runner that Open
// starts when WithStatistics is given.
type StatisticsOptions struct {
	// Clock drives the periodic run. Required.
	Clock clock.Clock
	// Interval is the time between periodic runs. Zero means
	// DefaultStatisticsInterval.
	Interval time.Duration
	// Logger receives one INFO line per run. Nil means slog.Default().
	Logger *slog.Logger

	// observe, when set by a test, receives each completed run after the
	// runner has rearmed its timer.
	observe func(statisticsRun)
}

// Option configures Open.
type Option func(*openOptions)

type openOptions struct {
	statistics *StatisticsOptions
	cache      CacheOptions
}

// WithStatistics starts the planner statistics runner. The runner refreshes
// the statistics once at start, every Interval, and on
// RequestStatisticsRefresh. Close stops it. Without this option the store
// refreshes the statistics only on Close.
func WithStatistics(options StatisticsOptions) Option {
	return func(open *openOptions) {
		open.statistics = &options
	}
}

// statisticsRun describes one completed refresh.
type statisticsRun struct {
	reason   string
	full     bool
	changed  bool
	duration time.Duration
	err      error
}

// statisticsRunner owns the goroutine that refreshes the planner statistics.
// Each run holds the single writer connection, so it runs between write
// transactions and never inside one. A normal run is bounded by
// statisticsAnalysisLimit and takes milliseconds.
type statisticsRunner struct {
	writer   *sql.DB
	clock    clock.Clock
	interval time.Duration
	logger   *slog.Logger
	observe  func(statisticsRun)
	requests chan struct{}
	cancel   context.CancelFunc
	done     chan struct{}
}

func newStatisticsRunner(writer *sql.DB, options StatisticsOptions) (*statisticsRunner, error) {
	if options.Clock == nil {
		return nil, errors.New("sqlite statistics: clock is required")
	}
	interval := options.Interval
	if interval <= 0 {
		interval = DefaultStatisticsInterval
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &statisticsRunner{
		writer:   writer,
		clock:    options.Clock,
		interval: interval,
		logger:   logger,
		observe:  options.observe,
		requests: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}, nil
}

// start launches the owner goroutine. stop ends it.
func (r *statisticsRunner) start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.run(ctx)
}

// stop cancels a run in progress and waits for the goroutine to return.
func (r *statisticsRunner) stop() {
	r.cancel()
	<-r.done
}

// request asks for a run soon. Requests that arrive while a run is pending
// coalesce into that run. request never blocks.
func (r *statisticsRunner) request() {
	select {
	case r.requests <- struct{}{}:
	default:
	}
}

func (r *statisticsRunner) run(ctx context.Context) {
	defer close(r.done)
	timer := r.clock.NewTimer(r.interval)
	defer timer.Stop()
	reason := "start"
	for {
		result := refreshStatistics(ctx, r.writer, r.clock, reason)
		if ctx.Err() != nil {
			return
		}
		logStatisticsRun(r.logger, result)
		timer.Stop()
		timer.Reset(r.interval)
		if r.observe != nil {
			r.observe(result)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			reason = "periodic"
		case <-r.requests:
			reason = "requested"
		}
	}
}

func logStatisticsRun(logger *slog.Logger, result statisticsRun) {
	mode := "optimize"
	if result.full {
		mode = "analyze"
	}
	if result.err != nil {
		logger.Warn("sqlite: planner statistics refresh failed", "reason", result.reason, "mode", mode, "duration", result.duration, "error", result.err)
		return
	}
	logger.Info("sqlite: planner statistics refreshed", "reason", result.reason, "mode", mode, "changed", result.changed, "duration", result.duration)
}

// refreshStatistics refreshes the planner statistics on the writer
// connection. It runs a full ANALYZE when the stored items statistics are
// missing or off by more than statisticsDriftFactor, and PRAGMA optimize
// otherwise. When sqlite_stat1 changes it forces every connection to reload
// its schema, because SQLite loads the statistics only at schema load and
// ANALYZE does not change the schema cookie: without the reload the reader
// pool keeps planning with the old statistics.
func refreshStatistics(ctx context.Context, writer *sql.DB, clk clock.Clock, reason string) statisticsRun {
	started := clk.Now()
	result := statisticsRun{reason: reason}
	result.full, result.changed, result.err = refreshStatisticsOnWriter(ctx, writer)
	result.duration = clk.Now().Sub(started)
	return result
}

func refreshStatisticsOnWriter(ctx context.Context, writer *sql.DB) (full, changed bool, err error) {
	conn, err := writer.Conn(ctx)
	if err != nil {
		return false, false, fmt.Errorf("acquire writer: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	full, err = statisticsNeedFullAnalyze(ctx, conn)
	if err != nil {
		return false, false, err
	}
	before, err := statisticsSnapshot(ctx, conn)
	if err != nil {
		return full, false, err
	}
	steps := []string{
		`PRAGMA analysis_limit = ` + strconv.Itoa(statisticsAnalysisLimit),
		`PRAGMA optimize = 0x10002`,
	}
	if full {
		steps = []string{`PRAGMA analysis_limit = 0`, `ANALYZE`}
	}
	for _, step := range steps {
		if _, err := conn.ExecContext(ctx, step); err != nil {
			return full, false, fmt.Errorf("%s: %w", step, err)
		}
	}
	after, err := statisticsSnapshot(ctx, conn)
	if err != nil {
		return full, false, err
	}
	if before == after {
		return full, false, nil
	}
	if err := reloadSchemaEverywhere(ctx, conn); err != nil {
		return full, true, err
	}
	return full, true, nil
}

// statisticsNeedFullAnalyze reports whether sqlite_stat1 has no row for a
// non-empty items table, or whether its stored items row count differs from
// the real count by more than statisticsDriftFactor.
func statisticsNeedFullAnalyze(ctx context.Context, conn *sql.Conn) (bool, error) {
	var hasItems, hasStat1 bool
	if err := conn.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'items'),
		EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&hasItems, &hasStat1); err != nil {
		return false, fmt.Errorf("read statistics tables: %w", err)
	}
	if !hasItems {
		return false, nil
	}
	var actual int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM items`).Scan(&actual); err != nil {
		return false, fmt.Errorf("count items: %w", err)
	}
	if !hasStat1 {
		return actual > 0, nil
	}
	// The first integer of a sqlite_stat1 stat is the number of rows the
	// index covers: the table row count for a full index, fewer for a partial
	// one. The largest is the table row count.
	rows, err := conn.QueryContext(ctx, `SELECT stat FROM sqlite_stat1 WHERE tbl = 'items'`)
	if err != nil {
		return false, fmt.Errorf("read items statistics: %w", err)
	}
	defer func() { _ = rows.Close() }()
	stored, found := int64(-1), false
	for rows.Next() {
		var stat string
		if err := rows.Scan(&stat); err != nil {
			return false, fmt.Errorf("read items statistics: %w", err)
		}
		found = true
		fields := strings.Fields(stat)
		if len(fields) == 0 {
			continue
		}
		if count, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			stored = max(stored, count)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read items statistics: %w", err)
	}
	if !found {
		return actual > 0, nil
	}
	if stored < 0 {
		return true, nil
	}
	return stored*statisticsDriftFactor < actual || actual*statisticsDriftFactor < stored, nil
}

// statisticsSnapshot returns the content of sqlite_stat1, or "" when the
// table does not exist.
func statisticsSnapshot(ctx context.Context, conn *sql.Conn) (string, error) {
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&exists); err != nil {
		return "", fmt.Errorf("read statistics table: %w", err)
	}
	if !exists {
		return "", nil
	}
	rows, err := conn.QueryContext(ctx, `SELECT tbl, COALESCE(idx, ''), COALESCE(stat, '') FROM sqlite_stat1 ORDER BY tbl, idx`)
	if err != nil {
		return "", fmt.Errorf("read statistics: %w", err)
	}
	defer rows.Close()
	var snapshot strings.Builder
	for rows.Next() {
		var table, index, stat string
		if err := rows.Scan(&table, &index, &stat); err != nil {
			return "", fmt.Errorf("scan statistics: %w", err)
		}
		snapshot.WriteString(table + "\x00" + index + "\x00" + stat + "\n")
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read statistics: %w", err)
	}
	return snapshot.String(), nil
}

// reloadSchemaEverywhere changes the schema cookie with a no-op schema change.
// Every connection then reloads its schema, and with it sqlite_stat1, before
// its next statement.
func reloadSchemaEverywhere(ctx context.Context, conn *sql.Conn) (err error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin statistics reload: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	for _, step := range []string{
		`CREATE TABLE IF NOT EXISTS deadfin_statistics_reload (id INTEGER)`,
		`DROP TABLE deadfin_statistics_reload`,
	} {
		if _, err := tx.ExecContext(ctx, step); err != nil {
			return fmt.Errorf("statistics reload: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit statistics reload: %w", err)
	}
	return nil
}
