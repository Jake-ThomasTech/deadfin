// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/playback"
)

const hlsBrokerURL = "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/abc/stream.ts"

type hlsLiveStreams map[string]playback.MediaSourceInfo

func (streams hlsLiveStreams) ChannelSources(context.Context, dto.UUID) ([]playback.MediaSourceInfo, bool, error) {
	return nil, false, nil
}

func (streams hlsLiveStreams) LiveStream(_ context.Context, id string) (playback.MediaSourceInfo, bool) {
	source, ok := streams[id]
	return source, ok
}

// hlsBrokerSource is an opened, probed Live TV source: the broker URL, no
// file facts and no run time.
func hlsBrokerSource() playback.MediaSourceInfo {
	source := progressiveCopySource(hlsBrokerURL)
	source.Protocol = playback.MediaProtocolHTTP
	source.Container = "mpegts"
	source.SourceRevision = ""
	source.FileFacts = playback.SourceFileFacts{}
	source.IsInfiniteStream = true
	source.LiveStreamID = "live1"
	source.IgnoreDts = true
	analyze := 3000
	source.AnalyzeDurationMs = &analyze
	return source
}

func newLiveHLSService(manager playback.HLSManager, streams hlsLiveStreams) *playback.HLSService {
	config := hlsServiceConfig(&progressiveFakeSource{err: errors.New("file source read for a live stream")}, manager)
	config.LiveTv = streams
	return playback.NewHLSService(config)
}

func hlsLiveRequest() playback.HLSRequest {
	request := hlsTranscodeRequest()
	request.LiveStreamID = "live1"
	return request
}

func TestHLSServiceLiveReadsOpenLiveStreamFromBroker(t *testing.T) {
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	service := newLiveHLSService(manager, hlsLiveStreams{"live1": hlsBrokerSource()})

	if _, err := service.Live(t.Context(), hlsLiveRequest()); err != nil {
		t.Fatalf("Live(): %v", err)
	}
	start := manager.lastStart
	if manager.startCalls != 1 || !start.Live {
		t.Fatalf("StartHLS = calls:%d request:%#v, want one live start", manager.startCalls, start)
	}
	joined := strings.Join(start.Args, " ")
	if !strings.Contains(joined, "-analyzeduration 3000000 -fflags +igndts -i "+hlsBrokerURL) {
		t.Errorf("argv = %s, want the broker URL with the live input modifiers", joined)
	}
	if start.OpenSource != nil || start.SourceRevision != "" {
		t.Errorf("live job pins a file: revision %q open %v", start.SourceRevision, start.OpenSource != nil)
	}
	if !start.Local {
		t.Error("live job may run on a transcode worker, which cannot reach the stream broker")
	}
}

func TestHLSServiceMasterPointsLiveStreamAtLivePlaylist(t *testing.T) {
	service := newLiveHLSService(&hlsFakeManager{}, hlsLiveStreams{"live1": hlsBrokerSource()})
	response, err := service.Master(t.Context(), hlsLiveRequest())
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if playlist := readHLSResponse(t, response); !strings.Contains(playlist, "\nlive.m3u8") {
		t.Errorf("master = %q, want the live.m3u8 variant", playlist)
	}
}

func TestHLSServiceRejectsUnresolvedLiveStream(t *testing.T) {
	for _, test := range []struct {
		name    string
		streams hlsLiveStreams
		want    error
	}{
		{name: "no live tv", want: playback.ErrProgressiveUnsupported},
		{name: "stream not open", streams: hlsLiveStreams{}, want: playback.ErrItemNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
			config := hlsServiceConfig(&progressiveFakeSource{source: progressiveCopySource("/media/a.ts")}, manager)
			if test.streams != nil {
				config.LiveTv = test.streams
			}
			_, err := playback.NewHLSService(config).Live(t.Context(), hlsLiveRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Live() error = %v, want %v", err, test.want)
			}
			if manager.startCalls != 0 {
				t.Errorf("manager starts = %d, want none", manager.startCalls)
			}
		})
	}
}
