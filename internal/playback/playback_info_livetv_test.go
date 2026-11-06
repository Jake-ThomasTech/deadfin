// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/playback"
)

// fakeLiveTv is one channel with one openable source.
type fakeLiveTv struct {
	channel dto.UUID
	busy    bool
	opens   []playback.OpenLiveStreamRequest
	open    map[string]playback.MediaSourceInfo
}

func (f *fakeLiveTv) ChannelSources(_ context.Context, itemID dto.UUID) ([]playback.MediaSourceInfo, bool, error) {
	if itemID != f.channel {
		return nil, false, nil
	}
	return []playback.MediaSourceInfo{liveSource("src", "")}, true, nil
}

func (f *fakeLiveTv) LiveStream(_ context.Context, id string) (playback.MediaSourceInfo, bool) {
	source, ok := f.open[id]
	return source, ok
}

func (f *fakeLiveTv) OpenLiveStream(_ context.Context, request playback.OpenLiveStreamRequest) (playback.LiveStreamResponse, error) {
	f.opens = append(f.opens, request)
	if f.busy {
		return playback.LiveStreamResponse{}, playback.ErrLiveStreamBusy
	}
	source := liveSource("src", "live1")
	f.open["live1"] = source
	return playback.LiveStreamResponse{MediaSource: source}, nil
}

func (f *fakeLiveTv) CloseLiveStream(context.Context, string) error { return nil }

func liveSource(id, liveStreamID string) playback.MediaSourceInfo {
	source := playback.MediaSourceInfo{
		ID:                   id,
		Protocol:             playback.MediaProtocolHTTP,
		IsInfiniteStream:     true,
		RequiresOpening:      true,
		RequiresClosing:      true,
		SupportsDirectStream: true,
		SupportsTranscoding:  true,
		OpenToken:            "token_" + id,
		MediaStreams: []playback.MediaStream{
			{Type: playback.StreamTypeVideo, Index: -1, IsInterlaced: true},
			{Type: playback.StreamTypeAudio, Index: -1},
		},
	}
	if liveStreamID != "" {
		source.LiveStreamID = liveStreamID
		source.Path = "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/" + liveStreamID + "/stream.ts"
	}
	return source
}

// liveRequest is a request that uses the stored transcoding profile.
func liveRequest(itemID dto.UUID) playback.PlaybackInfoRequest {
	request := playbackServiceRequest(itemID)
	request.DeviceProfile = nil
	return request
}

func newLiveTvService(live *fakeLiveTv) *playback.PlaybackInfoApplication {
	return playback.NewPlaybackInfoService(playback.PlaybackInfoServiceConfig{
		Sources:          playbackServiceSourceReader{},
		DeviceProfiles:   &playbackServiceProfileReader{profile: playbackServiceTranscodeProfile()},
		Permissions:      playbackServicePermissionReader{permissions: playbackServicePermissions()},
		Network:          playbackServiceNetwork{isLAN: true},
		NewPlaySessionID: func() string { return "live-session" },
		LiveStreams:      live,
		LiveTv:           live,
	})
}

// Without AutoOpenLiveStream a channel answers its unopened source.
func TestPlaybackInfoLiveTvChannelSource(t *testing.T) {
	live := &fakeLiveTv{channel: dto.UUID{9}, open: map[string]playback.MediaSourceInfo{}}
	service := newLiveTvService(live)
	got, err := service.PlaybackInfo(t.Context(), liveRequest(live.channel))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.MediaSources) != 1 || got.MediaSources[0].OpenToken != "token_src" || got.MediaSources[0].LiveStreamID != "" || len(live.opens) != 0 {
		t.Fatalf("response %+v opens %d", got, len(live.opens))
	}
}

// AutoOpenLiveStream opens the channel and answers only the opened source,
// with its LiveStreamId in the transcoding URL.
func TestPlaybackInfoLiveTvAutoOpen(t *testing.T) {
	live := &fakeLiveTv{channel: dto.UUID{9}, open: map[string]playback.MediaSourceInfo{}}
	service := newLiveTvService(live)
	request := liveRequest(live.channel)
	request.AutoOpenLiveStream = true
	got, err := service.PlaybackInfo(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.opens) != 1 || live.opens[0].OpenToken != "token_src" || live.opens[0].PlaySessionID != "live-session" {
		t.Fatalf("opens %+v", live.opens)
	}
	if len(got.MediaSources) != 1 || got.MediaSources[0].LiveStreamID != "live1" || got.PlaySessionID != "live-session" {
		t.Fatalf("response %+v", got)
	}
	if url := got.MediaSources[0].TranscodingURL; !strings.Contains(url, "LiveStreamId=live1") {
		t.Fatalf("transcoding url %q", url)
	}

	// A request with the LiveStreamId answers that stream without opening.
	request = liveRequest(live.channel)
	request.LiveStreamID = "live1"
	request.AutoOpenLiveStream = true
	got, err = service.PlaybackInfo(t.Context(), request)
	if err != nil || len(got.MediaSources) != 1 || got.MediaSources[0].LiveStreamID != "live1" || len(live.opens) != 1 {
		t.Fatalf("by live stream id: %+v %v opens %d", got, err, len(live.opens))
	}
	request.LiveStreamID = "gone"
	if _, err := service.PlaybackInfo(t.Context(), request); err == nil {
		t.Fatal("unknown live stream id succeeded")
	}
}

// A busy tuner answers NoCompatibleStream with no source, not a dead stream.
func TestPlaybackInfoLiveTvBusy(t *testing.T) {
	live := &fakeLiveTv{channel: dto.UUID{9}, busy: true, open: map[string]playback.MediaSourceInfo{}}
	service := newLiveTvService(live)
	request := liveRequest(live.channel)
	request.AutoOpenLiveStream = true
	got, err := service.PlaybackInfo(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != playback.PlaybackInfoNoCompatibleStream || len(got.MediaSources) != 0 || got.PlaySessionID == "" {
		t.Fatalf("busy response %+v", got)
	}
}
