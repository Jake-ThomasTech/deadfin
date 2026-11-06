// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
)

const (
	// liveTraceWindow is how long after the first live playlist fetch the
	// trace logs empty playlist polls and segment re-fetches.
	liveTraceWindow = 30 * time.Second
	// liveTraceWait is the shortest segment wait the trace logs: a segment
	// the client asked for before ffmpeg finished it.
	liveTraceWait = 100 * time.Millisecond
	// liveTraceIdle drops the trace of a playlist that has not been fetched
	// for this long.
	liveTraceIdle = 2 * time.Minute
)

// liveTraces records the startup of live HLS playlists of open live streams
// and logs it: one line for the first playlist fetch, one for the first
// segment fetch, and the empty polls and segment re-fetches of the first
// liveTraceWindow.
type liveTraces struct {
	clock  clock.Clock
	logger *slog.Logger

	mu    sync.Mutex
	byKey map[string]*liveTrace
}

// liveTrace is the trace of one playlist family, keyed by its output path.
type liveTrace struct {
	liveStreamID  string
	playSessionID string
	// requested is the first playlist fetch; seen is the latest fetch.
	requested time.Time
	seen      time.Time
	// next is the index after the newest complete segment of the last
	// playlist answer.
	next     int
	segments map[int]struct{}
}

// liveStartupSteps times one first live playlist fetch.
type liveStartupSteps struct {
	requested     time.Time
	ffmpegStarted time.Time
	firstSegment  time.Time
	ready         time.Time
}

func newLiveTraces(clk clock.Clock, logger *slog.Logger) *liveTraces {
	return &liveTraces{clock: clk, logger: logger, byKey: make(map[string]*liveTrace)}
}

// begin records a live playlist fetch. first reports the first fetch of the
// playlist family, which the caller times step by step.
func (traces *liveTraces) begin(key string, request HLSRequest) (now time.Time, first bool) {
	now = traces.clock.Now()
	traces.mu.Lock()
	defer traces.mu.Unlock()
	for other, trace := range traces.byKey {
		if now.Sub(trace.seen) >= liveTraceIdle {
			delete(traces.byKey, other)
		}
	}
	if trace := traces.byKey[key]; trace != nil {
		trace.seen = now
		return now, false
	}
	traces.byKey[key] = &liveTrace{
		liveStreamID:  request.LiveStreamID,
		playSessionID: request.PlaySessionID,
		requested:     now,
		seen:          now,
		next:          -1,
		segments:      make(map[int]struct{}),
	}
	return now, true
}

// startup logs the first playlist answer of a playlist family with the
// timings from PlaybackInfo to the ready playlist.
func (traces *liveTraces) startup(key string, request HLSRequest, open LiveStartup, steps liveStartupSteps, playlist string) {
	shape := parseLivePlaylist(playlist)
	traces.mu.Lock()
	if trace := traces.byKey[key]; trace != nil {
		trace.next = shape.next()
	}
	traces.mu.Unlock()

	attrs := []any{
		"liveStreamId", request.LiveStreamID,
		"playSessionId", request.PlaySessionID,
		"deviceId", request.DeviceID,
	}
	attrs = appendSpan(attrs, "connect", open.Received, open.Connected)
	attrs = appendSpan(attrs, "firstByte", open.Connected, open.FirstByte)
	attrs = appendSpan(attrs, "probe", open.FirstByte, open.Probed)
	attrs = appendSpan(attrs, "clientWait", open.Probed, steps.requested)
	attrs = appendSpan(attrs, "ffmpegStart", steps.requested, steps.ffmpegStarted)
	attrs = appendSpan(attrs, "firstSegment", steps.ffmpegStarted, steps.firstSegment)
	attrs = appendSpan(attrs, "playlistReady", steps.firstSegment, steps.ready)
	attrs = appendSpan(attrs, "total", open.Received, steps.ready)
	if open.Delivered > 0 {
		attrs = append(attrs, "inputBytes", open.Delivered)
		// inputRate is the upstream delivery rate since the first byte, in
		// bits per second. The source bitrate of a live stream is often an
		// estimate from the picture size, so the trace logs the rate itself.
		if elapsed := steps.ready.Sub(open.FirstByte).Seconds(); !open.FirstByte.IsZero() && elapsed > 0 {
			attrs = append(attrs, "inputRate", int64(float64(open.Delivered)*8/elapsed))
		}
	}
	attrs = append(attrs,
		"segments", shape.segments,
		"targetDuration", shape.targetDuration,
		"minSegments", request.MinSegments,
		"segmentLength", request.SegmentLengthSeconds,
	)
	traces.logger.Info("live hls startup", attrs...)
}

// poll records a later playlist answer and logs one that has no new
// segment within the trace window.
func (traces *liveTraces) poll(key string, now time.Time, playlist string) {
	shape := parseLivePlaylist(playlist)
	traces.mu.Lock()
	trace := traces.byKey[key]
	if trace == nil {
		traces.mu.Unlock()
		return
	}
	previous := trace.next
	trace.next = shape.next()
	since := now.Sub(trace.requested)
	liveStreamID, playSessionID := trace.liveStreamID, trace.playSessionID
	traces.mu.Unlock()
	if since < liveTraceWindow && previous >= 0 && shape.next() <= previous {
		traces.logger.Info("live hls playlist poll found no new segment",
			"liveStreamId", liveStreamID, "playSessionId", playSessionID,
			"sinceFirstPlaylist", since, "segments", shape.segments, "nextSegment", shape.next())
	}
}

// segment records a segment fetch and returns its time. It logs the first
// segment fetch of a playlist family and any re-fetch within the trace
// window.
func (traces *liveTraces) segment(key string, index int) time.Time {
	now := traces.clock.Now()
	traces.mu.Lock()
	trace := traces.byKey[key]
	if trace == nil {
		traces.mu.Unlock()
		return now
	}
	trace.seen = now
	since := now.Sub(trace.requested)
	if since >= liveTraceWindow {
		traces.mu.Unlock()
		return now
	}
	first := len(trace.segments) == 0
	_, again := trace.segments[index]
	trace.segments[index] = struct{}{}
	liveStreamID, playSessionID := trace.liveStreamID, trace.playSessionID
	traces.mu.Unlock()
	switch {
	case first:
		traces.logger.Info("live hls first segment request",
			"liveStreamId", liveStreamID, "playSessionId", playSessionID,
			"segment", index, "sinceFirstPlaylist", since)
	case again:
		traces.logger.Info("live hls segment fetched again",
			"liveStreamId", liveStreamID, "playSessionId", playSessionID,
			"segment", index, "sinceFirstPlaylist", since)
	}
	return now
}

// segmentServed logs a segment fetch, asked at asked, that waited for
// ffmpeg to finish the segment within the trace window.
func (traces *liveTraces) segmentServed(key string, index int, asked time.Time) {
	now := traces.clock.Now()
	waited := now.Sub(asked)
	if waited < liveTraceWait {
		return
	}
	traces.mu.Lock()
	trace := traces.byKey[key]
	if trace == nil || asked.Sub(trace.requested) >= liveTraceWindow {
		traces.mu.Unlock()
		return
	}
	liveStreamID, playSessionID, since := trace.liveStreamID, trace.playSessionID, asked.Sub(trace.requested)
	traces.mu.Unlock()
	traces.logger.Info("live hls segment request waited",
		"liveStreamId", liveStreamID, "playSessionId", playSessionID,
		"segment", index, "waited", waited, "sinceFirstPlaylist", since)
}

// appendSpan appends the time from one step to the next. A step that did
// not happen leaves the span out.
func appendSpan(attrs []any, name string, from, to time.Time) []any {
	if from.IsZero() || to.IsZero() {
		return attrs
	}
	return append(attrs, name, to.Sub(from))
}

// livePlaylistShape is what the trace reads from a live media playlist.
type livePlaylistShape struct {
	mediaSequence  int
	segments       int
	targetDuration int
}

// next is the index after the newest complete segment.
func (shape livePlaylistShape) next() int {
	return shape.mediaSequence + shape.segments
}

func parseLivePlaylist(playlist string) livePlaylistShape {
	var shape livePlaylistShape
	pending := false
	for line := range strings.Lines(playlist) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			shape.targetDuration, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"))
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			shape.mediaSequence, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
		case strings.HasPrefix(line, "#EXTINF:"):
			pending = true
		case pending && line != "" && !strings.HasPrefix(line, "#"):
			shape.segments++
			pending = false
		}
	}
	return shape
}
