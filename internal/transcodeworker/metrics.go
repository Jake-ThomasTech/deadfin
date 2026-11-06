// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Worker states in the scaling signals.
const (
	WorkerReady       = "ready"
	WorkerDraining    = "draining"
	WorkerQuarantined = "quarantined"
)

// Queue wait outcomes.
const (
	waitPlaced  = "placed"
	waitTimeout = "timeout"
)

// Job loss reasons in deadfin_transcode_worker_lost_jobs_total.
const (
	lossDisconnect = "disconnect"
	lossStall      = "stall"
	lossDrain      = "drain"
	lossMalformed  = "malformed"
)

var (
	waitBuckets   = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
	uploadBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
)

// PoolSignals are the scaling signals of one pool
// (docs/spikes/transcode-workers.md §3.9).
type PoolSignals struct {
	ActiveJobs int `json:"activeJobs"`
	QueuedJobs int `json:"queuedJobs"`
	// WarmingJobs counts the transcodes that PlaybackInfo announced and
	// that have no job yet (#623).
	WarmingJobs int `json:"warmingJobs"`
	// FreeSlots counts the free slots of ready workers only.
	FreeSlots int            `json:"freeSlots"`
	Workers   map[string]int `json:"workers"`
	// SlotsWanted is ActiveJobs + QueuedJobs + WarmingJobs + the pool's
	// warm slots: the one number an autoscaler targets.
	SlotsWanted       int     `json:"slotsWanted"`
	OldestWaitSeconds float64 `json:"oldestWaitSeconds"`
}

// Scaling is the scaling-signal document of /deadfin/v1/workers/scaling.
// AllPools ("*") covers the whole fleet.
type Scaling struct {
	Pools map[string]PoolSignals `json:"pools"`
}

// histogram is a cumulative Prometheus histogram.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *histogram) observe(value float64) {
	for index, bound := range h.bounds {
		if value <= bound {
			h.counts[index]++
		}
	}
	h.sum += value
	h.count++
}

func (h *histogram) clone() *histogram {
	return &histogram{bounds: h.bounds, counts: append([]uint64(nil), h.counts...), sum: h.sum, count: h.count}
}

// metrics are the hub's counters. hub.mu guards them.
type metrics struct {
	placements  map[[2]string]uint64 // target, rule
	fallbacks   map[[2]string]uint64 // target, reason
	lost        map[string]uint64    // reason
	waits       map[[2]string]*histogram
	uploadBytes uint64
	uploads     *histogram
}

func newMetrics() metrics {
	return metrics{
		placements: make(map[[2]string]uint64),
		fallbacks:  make(map[[2]string]uint64),
		lost:       make(map[string]uint64),
		waits:      make(map[[2]string]*histogram),
		uploads:    newHistogram(uploadBuckets),
	}
}

// observeWaitLocked records one queue wait in each of its pools.
func (m *metrics) observeWaitLocked(pools []string, outcome string, seconds float64) {
	for _, pool := range signalPools(pools) {
		key := [2]string{pool, outcome}
		h := m.waits[key]
		if h == nil {
			h = newHistogram(waitBuckets)
			m.waits[key] = h
		}
		h.observe(seconds)
	}
}

// signalPools are the pools a job counts in: its own, and the fleet. A job
// without pools may run on any worker, so it counts in the fleet only.
func signalPools(pools []string) []string {
	return append(append([]string(nil), pools...), AllPools)
}

// Scaling returns the scaling signals of every pool that a worker carries,
// a queued job waits for, or the policy names in warmSlots.
func (hub *Hub) Scaling() Scaling {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.scalingLocked()
}

func (hub *Hub) scalingLocked() Scaling {
	now := hub.config.Clock.Now()
	pools := map[string]*PoolSignals{}
	pool := func(name string) *PoolSignals {
		signals := pools[name]
		if signals == nil {
			signals = &PoolSignals{Workers: map[string]int{WorkerReady: 0, WorkerDraining: 0, WorkerQuarantined: 0}}
			pools[name] = signals
		}
		return signals
	}
	pool(AllPools)
	for name := range hub.policy.WarmSlots {
		pool(name)
	}
	for _, conn := range hub.workers {
		state := hub.stateLocked(conn, now)
		for _, name := range signalPools(conn.pools) {
			signals := pool(name)
			signals.Workers[state]++
			signals.ActiveJobs += len(conn.jobs)
			if state == WorkerReady && conn.caps.MaxJobs > len(conn.jobs) {
				signals.FreeSlots += conn.caps.MaxJobs - len(conn.jobs)
			}
		}
	}
	for _, waiter := range hub.queue {
		wait := now.Sub(waiter.since).Seconds()
		for _, name := range signalPools(waiter.pools) {
			signals := pool(name)
			signals.QueuedJobs++
			if wait > signals.OldestWaitSeconds {
				signals.OldestWaitSeconds = wait
			}
		}
	}
	if warming := hub.warmingLocked(now); warming > 0 {
		// A warm-up has no facts for the rules yet: it counts in the
		// default pools.
		for _, name := range signalPools(hub.policy.DefaultPools) {
			pool(name).WarmingJobs += warming
		}
	}
	result := Scaling{Pools: make(map[string]PoolSignals, len(pools))}
	for name, signals := range pools {
		signals.SlotsWanted = signals.ActiveJobs + signals.QueuedJobs + signals.WarmingJobs + hub.policy.warmSlots(name)
		result.Pools[name] = *signals
	}
	return result
}

// WriteMetrics writes the transcode-worker metrics in the Prometheus text
// format (version 0.0.4).
func (hub *Hub) WriteMetrics(w io.Writer) error {
	hub.mu.Lock()
	scaling := hub.scalingLocked()
	placements := copyCounters(hub.metrics.placements)
	fallbacks := copyCounters(hub.metrics.fallbacks)
	lost := make(map[string]uint64, len(hub.metrics.lost))
	for reason, value := range hub.metrics.lost {
		lost[reason] = value
	}
	waits := make(map[[2]string]*histogram, len(hub.metrics.waits))
	for key, h := range hub.metrics.waits {
		waits[key] = h.clone()
	}
	uploadBytes := hub.metrics.uploadBytes
	uploads := hub.metrics.uploads.clone()
	hub.mu.Unlock()

	out := bufio.NewWriter(w)
	names := make([]string, 0, len(scaling.Pools))
	for name := range scaling.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	gauge := func(metric, help string, value func(PoolSignals) float64) {
		fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s gauge\n", metric, help, metric)
		for _, name := range names {
			fmt.Fprintf(out, "%s{pool=%s} %s\n", metric, quoteLabel(name), formatFloat(value(scaling.Pools[name])))
		}
	}
	gauge("deadfin_transcode_pool_active_jobs", "Jobs running on workers of the pool.", func(p PoolSignals) float64 { return float64(p.ActiveJobs) })
	gauge("deadfin_transcode_pool_queued_jobs", "Jobs waiting for a worker of the pool.", func(p PoolSignals) float64 { return float64(p.QueuedJobs) })
	gauge("deadfin_transcode_pool_free_slots", "Free job slots of the ready workers of the pool.", func(p PoolSignals) float64 { return float64(p.FreeSlots) })
	gauge("deadfin_transcode_pool_slots_wanted", "Active plus queued jobs plus the warm slots: the autoscaling target.", func(p PoolSignals) float64 { return float64(p.SlotsWanted) })
	gauge("deadfin_transcode_pool_oldest_wait_seconds", "Wait of the oldest queued job of the pool.", func(p PoolSignals) float64 { return p.OldestWaitSeconds })
	fmt.Fprint(out, "# HELP deadfin_transcode_pool_workers Registered workers of the pool by state.\n# TYPE deadfin_transcode_pool_workers gauge\n")
	for _, name := range names {
		for _, state := range []string{WorkerReady, WorkerDraining, WorkerQuarantined} {
			fmt.Fprintf(out, "deadfin_transcode_pool_workers{pool=%s,state=%s} %d\n", quoteLabel(name), quoteLabel(state), scaling.Pools[name].Workers[state])
		}
	}
	fmt.Fprint(out, "# HELP deadfin_transcode_pool_wait_seconds Queue waits of the pool by outcome.\n# TYPE deadfin_transcode_pool_wait_seconds histogram\n")
	waitKeys := make([][2]string, 0, len(waits))
	for key := range waits {
		waitKeys = append(waitKeys, key)
	}
	sort.Slice(waitKeys, func(i, j int) bool {
		if waitKeys[i][0] != waitKeys[j][0] {
			return waitKeys[i][0] < waitKeys[j][0]
		}
		return waitKeys[i][1] < waitKeys[j][1]
	})
	for _, key := range waitKeys {
		writeHistogram(out, "deadfin_transcode_pool_wait_seconds", "pool="+quoteLabel(key[0])+",outcome="+quoteLabel(key[1]), waits[key])
	}
	counter := func(metric, help, first, second string, values map[[2]string]uint64) {
		fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s counter\n", metric, help, metric)
		keys := make([][2]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		for _, key := range keys {
			fmt.Fprintf(out, "%s{%s=%s,%s=%s} %d\n", metric, first, quoteLabel(key[0]), second, quoteLabel(key[1]), values[key])
		}
	}
	counter("deadfin_transcode_placements_total", "Transcode placements by target and rule.", "target", "rule", placements)
	counter("deadfin_transcode_fallback_total", "Placements that took the fallback chain, by target and reason.", "target", "reason", fallbacks)
	fmt.Fprint(out, "# HELP deadfin_transcode_worker_lost_jobs_total Worker jobs restarted elsewhere, by reason.\n# TYPE deadfin_transcode_worker_lost_jobs_total counter\n")
	for _, reason := range []string{lossDisconnect, lossDrain, lossMalformed, lossStall} {
		fmt.Fprintf(out, "deadfin_transcode_worker_lost_jobs_total{reason=%s} %d\n", quoteLabel(reason), lost[reason])
	}
	fmt.Fprintf(out, "# HELP deadfin_transcode_worker_upload_bytes_total Artifact bytes uploaded by workers.\n# TYPE deadfin_transcode_worker_upload_bytes_total counter\ndeadfin_transcode_worker_upload_bytes_total %d\n", uploadBytes)
	fmt.Fprint(out, "# HELP deadfin_transcode_worker_upload_seconds Duration of one complete artifact upload.\n# TYPE deadfin_transcode_worker_upload_seconds histogram\n")
	writeHistogram(out, "deadfin_transcode_worker_upload_seconds", "", uploads)
	return out.Flush()
}

func copyCounters(values map[[2]string]uint64) map[[2]string]uint64 {
	result := make(map[[2]string]uint64, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func writeHistogram(out io.Writer, metric, labels string, h *histogram) {
	prefix := labels
	if prefix != "" {
		prefix += ","
	}
	for index, bound := range h.bounds {
		fmt.Fprintf(out, "%s_bucket{%sle=%s} %d\n", metric, prefix, quoteLabel(formatFloat(bound)), h.counts[index])
	}
	fmt.Fprintf(out, "%s_bucket{%sle=\"+Inf\"} %d\n", metric, prefix, h.count)
	if labels == "" {
		fmt.Fprintf(out, "%s_sum %s\n%s_count %d\n", metric, formatFloat(h.sum), metric, h.count)
		return
	}
	fmt.Fprintf(out, "%s_sum{%s} %s\n%s_count{%s} %d\n", metric, labels, formatFloat(h.sum), metric, labels, h.count)
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// quoteLabel quotes a label value with the escapes of the text format.
func quoteLabel(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`
}
