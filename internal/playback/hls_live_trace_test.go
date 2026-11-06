// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// hlsStartupStreams is an open live stream that reports its startup times.
type hlsStartupStreams struct {
	hlsLiveStreams
	startup playback.LiveStartup
}

func (streams hlsStartupStreams) LiveStartup(id string) (playback.LiveStartup, bool) {
	_, ok := streams.hlsLiveStreams[id]
	return streams.startup, ok
}

// hlsTimedManager moves the fake clock as ffmpeg would take time: the start,
// the first segment and each further segment.
type hlsTimedManager struct {
	hlsFakeManager
	clock    *clock.Fake
	start    time.Duration
	segment0 time.Duration
	more     time.Duration
	waits    []int
	// segmentWait is how long each segment fetch waits for ffmpeg.
	segmentWait time.Duration
}

func (manager *hlsTimedManager) WaitSegment(ctx context.Context, path string, index int, timing *playback.HLSSegmentTiming) (io.ReadCloser, error) {
	manager.clock.Advance(manager.segmentWait)
	return manager.hlsFakeManager.WaitSegment(ctx, path, index, timing)
}

func (manager *hlsTimedManager) StartHLS(ctx context.Context, request playback.HLSJobRequest) error {
	manager.clock.Advance(manager.start)
	return manager.hlsFakeManager.StartHLS(ctx, request)
}

func (manager *hlsTimedManager) WaitPlaylist(ctx context.Context, path string, minSegments int) (string, error) {
	manager.waits = append(manager.waits, minSegments)
	if len(manager.waits) == 1 {
		manager.clock.Advance(manager.segment0)
	} else if len(manager.waits) == 2 {
		manager.clock.Advance(manager.more)
	}
	return manager.hlsFakeManager.WaitPlaylist(ctx, path, minSegments)
}

func traceLog(t *testing.T, log *bytes.Buffer, message string) string {
	t.Helper()
	for line := range strings.Lines(log.String()) {
		if strings.Contains(line, `msg="`+message+`"`) {
			return line
		}
	}
	t.Fatalf("no %q line in:\n%s", message, log.String())
	return ""
}

// The first live playlist fetch of an open live stream logs one line with
// the timings from PlaybackInfo to the ready playlist, the segment count and
// the target duration. In the first 30 s, a poll with no new segment, the
// first segment fetch and a segment re-fetch each log a line.
func TestHLSServiceLiveLogsStartupTrace(t *testing.T) {
	start := time.Unix(1000, 0)
	fake := clock.NewFake(start)
	playlist := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:3\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:3.000000,\nlive0.ts\n#EXTINF:3.000000,\nlive1.ts\n"
	manager := &hlsTimedManager{
		hlsFakeManager: hlsFakeManager{playlist: playlist, segment: "segment"},
		clock:          fake, start: 100 * time.Millisecond, segment0: 2 * time.Second, more: 3 * time.Second,
	}
	streams := hlsStartupStreams{
		hlsLiveStreams: hlsLiveStreams{"live1": hlsBrokerSource()},
		startup: playback.LiveStartup{
			Received:  start.Add(-6 * time.Second),
			Connected: start.Add(-5 * time.Second),
			FirstByte: start.Add(-4500 * time.Millisecond),
			Probed:    start.Add(-1500 * time.Millisecond),
			Delivered: 12_000_000,
		},
	}
	var log bytes.Buffer
	config := hlsServiceConfig(&progressiveFakeSource{err: errors.New("file source read for a live stream")}, manager)
	config.LiveTv = streams
	config.Clock = fake
	config.Logger = slog.New(slog.NewTextHandler(&log, nil))
	service := playback.NewHLSService(config)

	request := hlsLiveRequest()
	request.MinSegments, request.MinSegmentsSet = 2, true
	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("Live(): %v", err)
	}
	if len(manager.waits) != 2 || manager.waits[0] != 1 || manager.waits[1] != 2 {
		t.Fatalf("playlist waits = %v, want the first segment then the minimum", manager.waits)
	}
	line := traceLog(t, &log, "live hls startup")
	for _, field := range []string{
		"liveStreamId=live1", "playSessionId=session-9", "deviceId=device-7",
		"connect=1s", "firstByte=500ms", "probe=3s", "clientWait=1.5s",
		"ffmpegStart=100ms", "firstSegment=2s", "playlistReady=3s", "total=11.1s",
		"segments=2", "targetDuration=3", "minSegments=2",
		"inputBytes=12000000", "inputRate=10000000",
	} {
		if !strings.Contains(line, field) {
			t.Errorf("startup line lacks %s: %s", field, line)
		}
	}

	// A later fetch waits for the minimum only and finds no new segment.
	fake.Advance(time.Second)
	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("second Live(): %v", err)
	}
	if len(manager.waits) != 3 || manager.waits[2] != 2 {
		t.Fatalf("playlist waits = %v", manager.waits)
	}
	if line := traceLog(t, &log, "live hls playlist poll found no new segment"); !strings.Contains(line, "nextSegment=2") || !strings.Contains(line, "sinceFirstPlaylist=6.1s") {
		t.Errorf("poll line: %s", line)
	}
	if strings.Count(log.String(), "live hls startup") != 1 {
		t.Errorf("startup logged again:\n%s", log.String())
	}

	segment := request
	segment.Live, segment.SegmentIndex = true, 0
	fake.Advance(500 * time.Millisecond)
	fetch := func() {
		t.Helper()
		response, err := service.Segment(t.Context(), segment)
		if err != nil {
			t.Fatalf("Segment(): %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	fetch()
	if line := traceLog(t, &log, "live hls first segment request"); !strings.Contains(line, "segment=0") || !strings.Contains(line, "sinceFirstPlaylist=6.7s") {
		t.Errorf("first segment line: %s", line)
	}
	segment.SegmentIndex = 1
	fetch()
	if strings.Contains(log.String(), "fetched again") {
		t.Fatalf("a new segment logged as a re-fetch:\n%s", log.String())
	}
	segment.SegmentIndex = 0
	fetch()
	if line := traceLog(t, &log, "live hls segment fetched again"); !strings.Contains(line, "segment=0") {
		t.Errorf("re-fetch line: %s", line)
	}
	if strings.Contains(log.String(), "request waited") {
		t.Fatalf("a ready segment logged as a wait:\n%s", log.String())
	}

	// A segment the client asks for before ffmpeg finishes it waits.
	manager.segmentWait = 1500 * time.Millisecond
	segment.SegmentIndex = 2
	fetch()
	if line := traceLog(t, &log, "live hls segment request waited"); !strings.Contains(line, "segment=2") || !strings.Contains(line, "waited=1.5s") {
		t.Errorf("wait line: %s", line)
	}
	manager.segmentWait = 0

	// After the trace window (30 s from the first fetch), re-fetches and empty polls are not logged.
	before := log.Len()
	fake.Advance(30 * time.Second)
	fetch()
	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("late Live(): %v", err)
	}
	if log.Len() != before {
		t.Errorf("logged after the trace window:\n%s", log.String()[before:])
	}
}
