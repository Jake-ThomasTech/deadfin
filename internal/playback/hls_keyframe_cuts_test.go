// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/keyframes"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// hlsGOPManager serves each segment file as "g<index>;" so a test can see
// which files a segment body joins.
type hlsGOPManager struct {
	hlsFakeManager
	waits []int
}

func (manager *hlsGOPManager) WaitSegment(_ context.Context, _ string, index int, _ *playback.HLSSegmentTiming) (io.ReadCloser, error) {
	manager.waits = append(manager.waits, index)
	if manager.waitErr != nil && index > 0 {
		return nil, manager.waitErr
	}
	return io.NopCloser(strings.NewReader("g" + strconv.Itoa(index) + ";")), nil
}

// hlsKeyframeRemux is an MKV remux whose keyframe playlist with 6 s segments
// is [0,7.0) GOPs 0-1, [7.0,13.0) GOPs 2-4, [13.0,20.0) GOP 5, and
// [20.0,30.0) GOPs 6-7.
func hlsKeyframeRemux(t *testing.T) (*playback.HLSService, *hlsGOPManager, playback.HLSRequest) {
	t.Helper()
	source := progressiveCopySource("/media/feature.mkv")
	runtime := int64(300_000_000)
	source.RunTimeTicks = &runtime
	request := hlsTranscodeRequest()
	source.ID = request.MediaSourceID
	request.AllowVideoStreamCopy = true
	request.VideoBitrate = hlsInt(2_600_000)
	request.SegmentContainer = "mp4"
	request.SegmentLengthSeconds = 6
	manager := &hlsGOPManager{}
	config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
	config.Keyframes = &hlsFakeKeyframes{found: true, data: keyframes.Data{
		TotalDurationTicks: runtime,
		KeyframeTicks:      []int64{0, 30_000_000, 70_000_000, 90_000_000, 110_000_000, 130_000_000, 200_000_000, 202_000_000},
	}}
	return playback.NewHLSService(config), manager, request
}

// A remux job cut at every keyframe numbers its files by source GOP, so a
// restart in mid-file produces the advertised segments exactly. Segment N is
// the concatenation of its GOPs (deadfin#629).
func TestHLSServiceKeyframeRemuxJoinsSegmentGOPs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		index     int
		runtime   int64
		wantFirst int
		wantWaits []int
		wantBody  string
		wantSeek  string
	}{
		{name: "first segment starts at the first packet", index: 0, wantFirst: 0, wantWaits: []int{0, 1}, wantBody: "g0;g1;"},
		{name: "mid-file segment", index: 1, runtime: 70_000_000, wantFirst: 2, wantWaits: []int{2, 3, 4}, wantBody: "g2;g3;g4;", wantSeek: "00:00:07.500"},
		{name: "one-GOP segment", index: 2, runtime: 130_000_000, wantFirst: 5, wantWaits: []int{5}, wantBody: "g5;", wantSeek: "00:00:13.500"},
		{name: "short start GOP seeks to its middle", index: 3, runtime: 200_000_000, wantFirst: 6, wantWaits: []int{6, 7}, wantBody: "g6;g7;", wantSeek: "00:00:20.100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, manager, request := hlsKeyframeRemux(t)
			request.SegmentIndex = tc.index
			request.RuntimeTicks = tc.runtime

			response, err := service.Segment(t.Context(), request)
			if err != nil {
				t.Fatalf("Segment(%d): %v", tc.index, err)
			}
			if got := readHLSResponse(t, response); got != tc.wantBody {
				t.Errorf("segment %d body = %q, want GOP files %q", tc.index, got, tc.wantBody)
			}
			if got := manager.restartIndexes; len(got) != 1 || got[0] != tc.wantFirst {
				t.Errorf("RestartHLSAt indexes = %v, want the first GOP %d", got, tc.wantFirst)
			}
			if !equalInts(manager.waits, tc.wantWaits) {
				t.Errorf("WaitSegment indexes = %v, want %v", manager.waits, tc.wantWaits)
			}
			args := manager.lastRestart.Args
			if !hlsArgPair(args, "-hls_time", "0") || !hlsArgPair(args, "-start_number", strconv.Itoa(tc.wantFirst)) {
				t.Errorf("argv = %q, want -hls_time 0 and start number %d", args, tc.wantFirst)
			}
			if !hlsArgPair(manager.lastRestart.IdentityArgs, "-hls_time", "0") {
				t.Errorf("identity argv = %q, want the keyframe-cut rendition", manager.lastRestart.IdentityArgs)
			}
			if tc.wantSeek == "" {
				if hlsHasArg(args, "-ss") {
					t.Errorf("argv = %q, want no seek for the first GOP", args)
				}
			} else if !hlsArgPair(args, "-ss", tc.wantSeek) {
				t.Errorf("argv = %q, want seek %q inside the start GOP", args, tc.wantSeek)
			}
			if manager.lastRestart.MaxForwardGap < 1 {
				t.Errorf("MaxForwardGap = %d, want a GOP window", manager.lastRestart.MaxForwardGap)
			}
		})
	}
}

func TestHLSServiceKeyframeRemuxServesInitAndRejectsUnknownSegment(t *testing.T) {
	service, manager, request := hlsKeyframeRemux(t)
	request.SegmentIndex = -1
	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(-1): %v", err)
	}
	if got := readHLSResponse(t, response); got != "g-1;" || !equalInts(manager.restartIndexes, []int{-1}) {
		t.Errorf("init segment = %q restarts %v, want g-1 from a restart at -1", got, manager.restartIndexes)
	}

	request.SegmentIndex = 4
	if _, err := service.Segment(t.Context(), request); !errors.Is(err, playback.ErrProgressiveInvalid) {
		t.Fatalf("Segment(4) of a 4-segment playlist error = %v, want ErrProgressiveInvalid", err)
	}
}

func TestHLSServiceKeyframeRemuxClosesJoinedGOPsOnWaitFailure(t *testing.T) {
	service, manager, request := hlsKeyframeRemux(t)
	manager.waitErr = errors.New("ffmpeg exited")
	request.SegmentIndex = 0
	if _, err := service.Segment(t.Context(), request); !errors.Is(err, manager.waitErr) {
		t.Fatalf("Segment(0) error = %v, want the WaitSegment failure", err)
	}
}

// Only a video-copy VOD rendition with a keyframe playlist cuts at every
// keyframe. Transcodes and equal-length remux playlists keep hls_time.
func TestHLSServiceKeepsSegmentLengthCutsWithoutKeyframePlaylist(t *testing.T) {
	t.Run("transcode", func(t *testing.T) {
		service, manager, request := hlsKeyframeRemux(t)
		request.AllowVideoStreamCopy = false
		request.SegmentIndex = 1
		if _, err := service.Segment(t.Context(), request); err != nil {
			t.Fatalf("Segment(1): %v", err)
		}
		if !hlsArgPair(manager.lastRestart.Args, "-hls_time", "6") || !equalInts(manager.waits, []int{1}) {
			t.Errorf("transcode argv = %q waits %v, want -hls_time 6 and segment file 1", manager.lastRestart.Args, manager.waits)
		}
	})
	t.Run("remux without keyframes", func(t *testing.T) {
		source := progressiveCopySource("/media/feature.mp4")
		request := hlsTranscodeRequest()
		request.AllowVideoStreamCopy = true
		request.VideoBitrate = hlsInt(2_600_000)
		request.SegmentIndex = 1
		manager := &hlsGOPManager{}
		config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
		config.Keyframes = &hlsFakeKeyframes{found: true, data: keyframes.Data{TotalDurationTicks: 300_000_000, KeyframeTicks: []int64{0, 70_000_000}}}
		if _, err := playback.NewHLSService(config).Segment(t.Context(), request); err != nil {
			t.Fatalf("Segment(1): %v", err)
		}
		if !hlsArgPair(manager.lastRestart.Args, "-hls_time", "6") || !equalInts(manager.waits, []int{1}) {
			t.Errorf("MP4 remux argv = %q waits %v, want -hls_time 6 and segment file 1", manager.lastRestart.Args, manager.waits)
		}
		if got := manager.lastRestart.SegmentStartTicks; got != nil {
			t.Errorf("SegmentStartTicks = %v, want nil for segment-length files", got)
		}
	})
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// The manager gets the GOP boundaries of a keyframe-cut job, so its segment
// cleaner expires files by GOP end, and a restart seeks without accurate seek
// so its audio starts at the start keyframe (deadfin#799).
func TestHLSServiceKeyframeRemuxPassesGOPBoundsToManager(t *testing.T) {
	service, manager, request := hlsKeyframeRemux(t)
	request.SegmentIndex = 2
	request.RuntimeTicks = 130_000_000
	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(2): %v", err)
	}
	_ = readHLSResponse(t, response)
	job := manager.lastRestart
	want := []int64{0, 30_000_000, 70_000_000, 90_000_000, 110_000_000, 130_000_000, 200_000_000, 202_000_000}
	if len(job.SegmentStartTicks) != len(want) {
		t.Fatalf("SegmentStartTicks = %v, want the keyframes %v", job.SegmentStartTicks, want)
	}
	for index := range want {
		if job.SegmentStartTicks[index] != want[index] {
			t.Fatalf("SegmentStartTicks = %v, want the keyframes %v", job.SegmentStartTicks, want)
		}
	}
	if !hlsHasArg(job.Args, "-noaccurate_seek") {
		t.Errorf("argv = %q, want -noaccurate_seek for a mid-file restart", job.Args)
	}
	if hlsHasArg(job.IdentityArgs, "-noaccurate_seek") {
		t.Errorf("identity argv = %q, want no per-request seek options", job.IdentityArgs)
	}
}
