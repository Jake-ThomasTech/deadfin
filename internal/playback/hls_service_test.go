// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/api/bind"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/keyframes"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

// These tests exercise the handler-facing HLS orchestration seam. Source
// opening remains manager-owned; the fake manager opens only when a test asks it to.
func TestHLSServiceMasterSelectsRequestedSourceAndUsesRenditionPlaylistIdentity(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/媒体/élan.mp4")}
	manager := &hlsFakeManager{}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.UserAgent = "Jellyfin/10.10 β"
	request.DeviceID = "装置-7"
	request.PlaySessionID = "会话-9"
	request.RawQuery = "api_key=token-3&MediaSourceId=source-9"

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if source.calls != 1 || source.userID != request.UserID || source.itemID != request.ItemID || source.mediaSourceID != request.MediaSourceID {
		t.Fatalf("StaticSource call = calls:%d user:%s item:%s source:%q, want exactly request identity", source.calls, source.userID, source.itemID, source.mediaSourceID)
	}
	if !strings.HasPrefix(response.OutputPath, "/cache/") || !strings.HasSuffix(response.OutputPath, ".m3u8") {
		t.Errorf("Master().OutputPath = %q, want a cache-local rendition playlist", response.OutputPath)
	}
	if response.ContentType != "application/x-mpegURL" {
		t.Errorf("Master().ContentType = %q, want HLS media type", response.ContentType)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "#EXTM3U\n") || !strings.Contains(playlist, "main.m3u8?api_key=token-3&MediaSourceId=source-9") {
		t.Errorf("Master() playlist = %q, want projected master URI retaining raw caller query", playlist)
	}
	if manager.startCalls != 0 || manager.waitSegmentCalls != 0 || manager.waitPlaylistCalls != 0 {
		t.Errorf("Master() manager calls = start:%d segment:%d playlist:%d, want no transcode lifecycle for a pure projection", manager.startCalls, manager.waitSegmentCalls, manager.waitPlaylistCalls)
	}
}

func TestHLSServiceMasterUsesSelectedSourceRFC6381CodecsForStreamCopy(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	service := newHLSService(source, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.AllowVideoStreamCopy = true
	request.AllowAudioStreamCopy = true
	request.EnableAutoStreamCopy = true
	request.VideoBitrate = hlsInt(2_600_000)
	request.AudioBitrate = hlsInt(384_000)
	request.MaxAudioChannels = hlsInt(6)
	request.AudioCodec = "ac3"
	request.AudioCodecs = []string{"ac3"}

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "CODECS=\"avc1.640029,ac-3\"") {
		t.Errorf("stream-copy Master() playlist = %q, want selected source H264 High@4.1 and AC3 RFC 6381 codecs", playlist)
	}
	if strings.Contains(playlist, "CODECS=\"copy") {
		t.Errorf("stream-copy Master() playlist = %q, must not expose encoder sentinel codec copy", playlist)
	}
}

func TestHLSServiceMasterUsesHEVCHighTierForStreamCopy(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/hevc-high-tier.mkv")
	video := &sourceInfo.MediaStreams[0]
	profile, codec, bitDepth := "Main 10", "hevc", 10
	level := 153.0
	video.Codec, video.Profile, video.Level = codec, &profile, &level
	video.BitDepth, video.HighTier = &bitDepth, true
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsMasterCopyRequest("hevc")

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "CODECS=\"hvc1.2.4.H153.B0,mp4a.40.2\"") {
		t.Errorf("stream-copy Master() playlist = %q, want high-tier HEVC Main 10 CODECS token", playlist)
	}
}

func TestHLSServiceMasterProjectsSelectedVP9PixelFormatAndGeometry(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/vp9-feature.webm")
	video := &sourceInfo.MediaStreams[0]
	pixelFormat := "yuv444p"
	width, height, bitrate := 1920, 1080, 4_000_000
	video.Codec = "vp9"
	video.PixelFormat = &pixelFormat
	video.Width = &width
	video.Height = &height
	video.BitRate = &bitrate

	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.AllowVideoStreamCopy = true
	request.EnableAutoStreamCopy = true
	request.VideoCodec = "vp9"
	request.VideoCodecs = []string{"vp9"}
	request.VideoBitrate = hlsInt(4_000_000)
	request.MaxWidth = hlsInt(1920)
	request.MaxHeight = hlsInt(1080)
	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "vp09.01.40.08") {
		t.Errorf("Master() playlist = %q, want selected VP9 yuv444p source projected as profile 01 with level 40 from 1920x1080@30 geometry", playlist)
	}
}

func TestHLSServiceVP9HDRRangeConstraintForcesTranscode(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/vp9-hdr.webm")
	video := &sourceInfo.MediaStreams[0]
	transfer, pixelFormat := "smpte2084", "yuv420p10le"
	bitDepth := 10
	video.Codec = "vp9"
	video.ColorTransfer = &transfer
	video.PixelFormat = &pixelFormat
	video.BitDepth = &bitDepth

	manager := &hlsFakeManager{segment: "segment"}
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)
	request := hlsTranscodeRequest()
	request.AllowVideoStreamCopy = true
	request.EnableAutoStreamCopy = true
	request.VideoCodec = "vp9"
	request.VideoCodecs = []string{"vp9", "h264"}
	request.VideoBitrate = hlsInt(2_600_000)
	request.StreamOptions = playback.NewStreamOptions()
	request.StreamOptions.Set("vp9-rangetype", "SDR")

	if _, err := service.Segment(t.Context(), request); err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-codec:v:0", "libx264") || hlsArgPair(manager.lastRestart.Args, "-codec:v:0", "copy") {
		t.Errorf("VP9 HDR segment argv = %q, want x264 transcode when negotiated vp9-rangetype excludes HDR", manager.lastRestart.Args)
	}
}

func TestHLSServiceRejectsIncompatibleVP9WhenNoH264FallbackWasNegotiated(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/vp9-hdr.webm")
	video := &sourceInfo.MediaStreams[0]
	transfer, pixelFormat := "smpte2084", "yuv420p10le"
	bitDepth := 10
	video.Codec = "vp9"
	video.ColorTransfer = &transfer
	video.PixelFormat = &pixelFormat
	video.BitDepth = &bitDepth

	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.AllowVideoStreamCopy = true
	request.EnableAutoStreamCopy = true
	request.VideoCodec = "vp9"
	request.VideoCodecs = []string{"vp9"}
	request.VideoBitrate = hlsInt(2_600_000)
	request.StreamOptions = playback.NewStreamOptions()
	request.StreamOptions.Set("vp9-rangetype", "SDR")

	_, err := service.Segment(t.Context(), request)
	if !errors.Is(err, playback.ErrProgressiveUnsupported) {
		t.Fatalf("Segment() incompatible VP9-only request error = %v, want ErrProgressiveUnsupported", err)
	}
}

func TestHLSServiceMasterProjectsSelectedHDR10PlusSourceFact(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/hdr10plus-feature.mp4")
	video := &sourceInfo.MediaStreams[0]
	transfer, pixelFormat := "smpte2084", "yuv420p10le"
	hdr10Plus := true
	video.ColorTransfer = &transfer
	video.PixelFormat = &pixelFormat
	video.Hdr10PlusPresentFlag = &hdr10Plus

	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.AllowVideoStreamCopy = true
	request.EnableAutoStreamCopy = true
	request.VideoBitrate = hlsInt(2_600_000)

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "VIDEO-RANGE=PQ") || !strings.Contains(playlist, "SUPPLEMENTAL-CODECS=\"avc1.640029/cdm4\"") {
		t.Errorf("Master() playlist = %q, want HDR10+ selected-source range and cdm4 supplemental codec metadata", playlist)
	}
}

func TestHLSServiceMasterProjectsEnabledTextSubtitleRendition(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/subtitled-feature.mp4")
	language := "eng"
	sourceInfo.MediaStreams = append(sourceInfo.MediaStreams, playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "srt", Language: &language, IsForced: true})
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.EnableSubtitlesInManifest = true
	request.APIKey = "token-3"
	subtitleIndex := 2
	delivery := playback.SubtitleHLS
	request.SubtitleStreamIndex = &subtitleIndex
	request.SubtitleDeliveryMethod = &delivery

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"Eng - Forced - SRT\",DEFAULT=YES,FORCED=YES") || !strings.Contains(playlist, "URI=\""+request.MediaSourceID+"/Subtitles/2/subtitles.m3u8?SegmentLength=30&ApiKey=token-3\"") || !strings.Contains(playlist, "LANGUAGE=\"eng\"") || !strings.Contains(playlist, "SUBTITLES=\"subs\"") {
		t.Errorf("Master() playlist = %q, want enabled selected text subtitle rendition and variant subtitle group", playlist)
	}
}

// Every playlist URI is a later authenticated request. Its API key must remain
// one opaque query value even when the credential contains reserved bytes.
func TestHLSServiceMasterEscapesOpaqueCredentialsInMasterSubtitleAndTrickplayURIs(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/opaque-credential-feature.mp4")
	language := "eng"
	sourceInfo.MediaStreams = append(sourceInfo.MediaStreams, playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "srt", Language: &language})
	trickplays := &hlsFakeTrickplays{metadata: []playback.HLSTrickplayMetadata{{Width: 320, Height: 180, Bandwidth: 64_000}}}
	config := hlsServiceConfig(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	config.Trickplays = trickplays
	service := playback.NewHLSService(config)
	credential := "token +&=✓/%"
	request := hlsTranscodeRequest()
	request.APIKey = credential
	request.RawQuery = "ApiKey=" + url.QueryEscape(credential)
	request.EnableTrickplay = true
	request.EnableSubtitlesInManifest = true
	subtitleIndex := 2
	delivery := playback.SubtitleHLS
	request.SubtitleStreamIndex = &subtitleIndex
	request.SubtitleDeliveryMethod = &delivery

	response, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	for name, uri := range map[string]string{
		"master":    hlsURIWithPrefix(t, playlist, "main.m3u8?"),
		"subtitle":  hlsAttributeURI(t, playlist, "#EXT-X-MEDIA:TYPE=SUBTITLES"),
		"trickplay": hlsAttributeURI(t, playlist, "#EXT-X-IMAGE-STREAM-INF:"),
	} {
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatalf("parse %s URI %q: %v", name, uri, err)
		}
		if got := parsed.Query().Get("ApiKey"); got != credential {
			t.Errorf("%s URI API key = %q, want opaque credential %q; URI=%q", name, got, credential, uri)
		}
	}
}

func TestHLSServiceRejectsSegmentLengthThatOverflowsMilliseconds(t *testing.T) {
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.SegmentLengthSeconds = int(^uint(0) >> 1)

	_, err := service.Main(t.Context(), request)
	if !errors.Is(err, playback.ErrProgressiveInvalid) {
		t.Fatalf("Main() oversized segment duration error = %v, want ErrProgressiveInvalid", err)
	}
}

func TestHLSServicePreservesSourceFailureWithoutStartingManager(t *testing.T) {
	sourceErr := errors.New("source unavailable")
	source := &progressiveFakeSource{err: sourceErr}
	manager := &hlsFakeManager{}
	service := newHLSService(source, manager)

	_, err := service.Main(t.Context(), hlsTranscodeRequest())
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Main() error = %v, want original source error %v", err, sourceErr)
	}
	if manager.startCalls != 0 || manager.waitSegmentCalls != 0 || manager.waitPlaylistCalls != 0 {
		t.Errorf("source failure manager calls = start:%d segment:%d playlist:%d, want none", manager.startCalls, manager.waitSegmentCalls, manager.waitPlaylistCalls)
	}
}

func TestHLSServiceSurfacesManagerFailuresAtTheOwningOperation(t *testing.T) {
	restartErr := errors.New("restart rejected")
	waitErr := errors.New("segment not ready")
	liveErr := errors.New("live playlist not ready")
	for _, tc := range []struct {
		name    string
		manager *hlsFakeManager
		invoke  func(*playback.HLSService, playback.HLSRequest) error
		want    error
	}{
		{
			name:    "restart",
			manager: &hlsFakeManager{restartErr: restartErr},
			invoke: func(service *playback.HLSService, request playback.HLSRequest) error {
				_, err := service.Segment(t.Context(), request)
				return err
			},
			want: restartErr,
		},
		{
			name:    "segment readiness",
			manager: &hlsFakeManager{waitErr: waitErr},
			invoke: func(service *playback.HLSService, request playback.HLSRequest) error {
				_, err := service.Segment(t.Context(), request)
				return err
			},
			want: waitErr,
		},
		{
			name:    "live playlist readiness",
			manager: &hlsFakeManager{waitErr: liveErr},
			invoke: func(service *playback.HLSService, request playback.HLSRequest) error {
				request.Live = true
				_, err := service.Live(t.Context(), request)
				return err
			},
			want: liveErr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, tc.manager)
			if err := tc.invoke(service, hlsTranscodeRequest()); !errors.Is(err, tc.want) {
				t.Fatalf("HLS manager %s error = %v, want original %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestHLSServiceProjectsMainPlaylistWithCallerResolvedQueryAndFMP4Init(t *testing.T) {
	for _, tc := range []struct {
		name, container string
		want            []string
	}{
		{
			name:      "transport stream segments",
			container: "ts",
			want: []string{
				"#EXT-X-VERSION:3\n",
				"hls/main/0.ts?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=60000000",
			},
		},
		{
			name:      "fragmented MP4 segments include init minus one",
			container: "fmp4",
			want: []string{
				"#EXT-X-VERSION:7\n",
				"#EXT-X-MAP:URI=\"hls/main/-1.mp4?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=0\"",
				"hls/main/0.mp4?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=60000000",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/feature.mp4")
			runtime := int64(130_000_000)
			sourceInfo.RunTimeTicks = &runtime
			source := &progressiveFakeSource{source: sourceInfo}
			manager := &hlsFakeManager{}
			service := newHLSService(source, manager)
			request := hlsTranscodeRequest()
			request.SegmentContainer = tc.container
			request.SegmentLengthSeconds = 6
			request.RawQuery = "api_key=token-3"
			request.EndpointPrefix = "hls/main/"

			response, err := service.Main(t.Context(), request)
			if err != nil {
				t.Fatalf("Main(): %v", err)
			}
			playlist := readHLSResponse(t, response)
			for _, fragment := range tc.want {
				if !strings.Contains(playlist, fragment) {
					t.Errorf("Main() playlist = %q, want fragment %q", playlist, fragment)
				}
			}
			if manager.startCalls != 0 || manager.waitPlaylistCalls != 0 {
				t.Errorf("Main() manager calls = start:%d playlist:%d, want pure projected VOD playlist", manager.startCalls, manager.waitPlaylistCalls)
			}
		})
	}
}

func TestHLSServiceMainReplacesStaleTimingQueryFacts(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/feature.mp4")
	runtime := int64(130_000_000)
	sourceInfo.RunTimeTicks = &runtime
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.RawQuery = "api_key=token-3&runtimeTicks=999&actualSegmentLengthTicks=111"

	response, err := service.Main(t.Context(), request)
	if err != nil {
		t.Fatalf("Main(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	const want = "hls/main/0.ts?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=60000000"
	if !strings.Contains(playlist, want) || strings.Contains(playlist, "runtimeTicks=999") || strings.Contains(playlist, "actualSegmentLengthTicks=111") {
		t.Errorf("Main() playlist = %q, want generated segment timing to replace stale caller timing", playlist)
	}
}

func TestHLSServiceMainDropsPercentEncodedTimingAliasesBeforeSegmentBinding(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/feature.mp4")
	runtime := int64(130_000_000)
	sourceInfo.RunTimeTicks = &runtime
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	request := hlsTranscodeRequest()
	request.RawQuery = "api_key=token-3&%72untimeTicks=999&preserved=%2Fraw%2F&%61ctualSegmentLengthTicks=111&tail=one%2Btwo"

	response, err := service.Main(t.Context(), request)
	if err != nil {
		t.Fatalf("Main(): %v", err)
	}
	playlist := readHLSResponse(t, response)
	const wantURI = "hls/main/0.ts?api_key=token-3&preserved=%2Fraw%2F&tail=one%2Btwo&runtimeTicks=0&actualSegmentLengthTicks=60000000"
	segmentURI := ""
	for line := range strings.SplitSeq(playlist, "\n") {
		if strings.HasPrefix(line, "hls/main/0.ts?") {
			segmentURI = line
			break
		}
	}
	if segmentURI == "" {
		t.Fatalf("Main() playlist = %q, want a segment URI", playlist)
	}
	if segmentURI != wantURI {
		t.Errorf("Main() segment URI = %q, want %q with aliases removed and unrelated raw query components preserved", segmentURI, wantURI)
	}

	segmentRequest := httptest.NewRequest("GET", "http://example.test/"+segmentURI, nil)
	var timing struct {
		RuntimeTicks             *int64 `query:"runtimeTicks"`
		ActualSegmentLengthTicks *int64 `query:"actualSegmentLengthTicks"`
	}
	if err := bind.QueryRequest(segmentRequest, &timing); err != nil {
		t.Fatalf("binding computed segment timing: %v", err)
	}
	gotRuntimeTicks, gotLengthTicks := int64(-1), int64(-1)
	if timing.RuntimeTicks != nil {
		gotRuntimeTicks = *timing.RuntimeTicks
	}
	if timing.ActualSegmentLengthTicks != nil {
		gotLengthTicks = *timing.ActualSegmentLengthTicks
	}
	if gotRuntimeTicks != 0 || gotLengthTicks != 60_000_000 {
		t.Errorf("bound segment timing = runtime:%d length:%d, want runtime:0 length:60000000", gotRuntimeTicks, gotLengthTicks)
	}
}

func TestHLSServiceLiveAcceptsSelectedInfiniteFileSource(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/recording-in-progress.ts")
	sourceInfo.IsInfiniteStream = true
	sourceInfo.LiveStreamID = "selected-file-live"
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)
	request := hlsTranscodeRequest()
	request.Live = true

	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("Live() selected infinite local file source: %v", err)
	}
	if manager.startCalls != 1 || !manager.lastStart.Live || !hlsArgPair(manager.lastStart.Args, "-hls_playlist_type", "event") {
		t.Errorf("Live() manager job = %#v, want one EVENT HLS start for selected infinite file source", manager.lastStart)
	}
}

// hlsLiveTvStub resolves every live stream id to one open source.
type hlsLiveTvStub struct{ source playback.MediaSourceInfo }

func (hlsLiveTvStub) ChannelSources(context.Context, dto.UUID) ([]playback.MediaSourceInfo, bool, error) {
	return nil, false, nil
}

func (stub hlsLiveTvStub) LiveStream(context.Context, string) (playback.MediaSourceInfo, bool) {
	return stub.source, true
}

// A live probe can detect a video stream without its dimensions when the
// source joins mid-GOP. The transcode keeps the source size, and the master
// playlist omits RESOLUTION because it is unknown.
func TestHLSServiceLiveTranscodesSourceWithoutProbedDimensions(t *testing.T) {
	audioBitrate, channels := 128_000, 2
	source := playback.MediaSourceInfo{
		Protocol:     playback.MediaProtocolHTTP,
		Path:         "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/stream-1/stream.ts",
		Container:    "mpegts",
		LiveStreamID: "live-1",
		MediaStreams: []playback.MediaStream{
			{Type: playback.StreamTypeVideo, Index: 0, Codec: "mpeg2video"},
			{Type: playback.StreamTypeAudio, Index: 1, Codec: "aac", BitRate: &audioBitrate, Channels: &channels, IsDefault: true},
		},
	}
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	config := hlsServiceConfig(&progressiveFakeSource{}, manager)
	config.LiveTv = hlsLiveTvStub{source: source}
	service := playback.NewHLSService(config)
	request := hlsTranscodeRequest()
	request.Live = true
	request.LiveStreamID = "live-1"
	request.MaxWidth, request.MaxHeight = nil, nil
	request.AudioStreamIndex = hlsInt(1)

	master, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("Master() live source without dimensions: %v", err)
	}
	if playlist := readHLSResponse(t, master); strings.Contains(playlist, ",RESOLUTION=") {
		t.Errorf("Master() playlist = %q, want no RESOLUTION without a known output size", playlist)
	}

	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("Live() live source without dimensions: %v", err)
	}
	filter := ""
	for index := 0; index+1 < len(manager.lastStart.Args); index++ {
		if manager.lastStart.Args[index] == "-vf" {
			filter = manager.lastStart.Args[index+1]
		}
	}
	if !strings.Contains(filter, "scale=trunc(960/2)*2:-2") {
		t.Errorf("Live() -vf = %q, want an aspect-preserving width scale without a height", filter)
	}
}

func TestHLSServiceLiveDefersProviderBackedLiveStreamRequest(t *testing.T) {
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/recording-in-progress.ts")}, manager)
	request := hlsTranscodeRequest()
	request.Live = true
	request.LiveStreamID = "provider-live"

	_, err := service.Live(t.Context(), request)
	if !errors.Is(err, playback.ErrProgressiveUnsupported) {
		t.Fatalf("Live() provider-backed request error = %v, want ErrProgressiveUnsupported", err)
	}
	if manager.startCalls != 0 {
		t.Errorf("provider-backed Live() manager starts = %d, want none", manager.startCalls)
	}
}

func TestHLSServiceLiveDelegatesReadinessAndRewritesFMP4InitWithoutFilesystemAccess(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/live-input.mp4")}
	manager := &hlsFakeManager{playlist: "#EXTM3U\n#EXT-X-MAP:URI=\"live-identity-1.mp4\"\n#EXTINF:3.0,\nlive-identity0.mp4\n"}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.SegmentContainer = "fmp4"
	request.Live = true
	request.MinSegments = 3

	response, err := service.Live(t.Context(), request)
	if err != nil {
		t.Fatalf("Live(): %v", err)
	}
	if manager.startCalls != 1 || !manager.lastStart.Live {
		t.Fatalf("Live() StartHLS = calls:%d request:%#v, want one live HLS start", manager.startCalls, manager.lastStart)
	}
	if !hlsArgPair(manager.lastStart.Args, "-hls_playlist_type", "event") || hlsArgPair(manager.lastStart.Args, "-hls_playlist_type", "vod") {
		t.Errorf("Live() argv = %q, want event playlist type and no VOD type", manager.lastStart.Args)
	}
	playlistID := strings.TrimSuffix(strings.TrimPrefix(response.OutputPath, "/cache/"), ".m3u8")
	if !hlsArgPair(manager.lastStart.Args, "-hls_base_url", "hls/"+playlistID+"/") {
		t.Errorf("Live() argv = %q, want builder-owned live base URL hls/%s/", manager.lastStart.Args, playlistID)
	}
	if manager.waitPlaylistCalls != 1 || manager.waitPlaylistPath != response.OutputPath || manager.waitPlaylistMinSegments != request.MinSegments {
		t.Fatalf("Live() readiness = calls:%d path:%q min:%d, want manager-owned playlist readiness for response path and request minimum", manager.waitPlaylistCalls, manager.waitPlaylistPath, manager.waitPlaylistMinSegments)
	}
	playlist := readHLSResponse(t, response)
	if !strings.Contains(playlist, "#EXT-X-MAP:URI=\"hls/live-identity/live-identity-1.mp4\"") {
		t.Errorf("Live() playlist = %q, want fMP4 init URI rewritten through handler path", playlist)
	}
}

func TestHLSServiceLivePermitsZeroMinimumSegmentsWhenPlaylistExists(t *testing.T) {
	manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/live-input.mp4")}, manager)
	request := hlsTranscodeRequest()
	request.Live = true
	request.MinSegments = 0
	request.MinSegmentsSet = true

	if _, err := service.Live(t.Context(), request); err != nil {
		t.Fatalf("Live() with omitted minimum segments: %v", err)
	}
	if manager.waitPlaylistCalls != 1 || manager.waitPlaylistMinSegments != 0 {
		t.Errorf("Live() readiness = calls:%d min:%d, want one existing-playlist wait with zero minimum", manager.waitPlaylistCalls, manager.waitPlaylistMinSegments)
	}
}

func TestHLSServiceProjectsLiveAndVODTranscodePresets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       bool
		wantPreset string
		invoke     func(*playback.HLSService, playback.HLSRequest) error
		args       func(*hlsFakeManager) []string
	}{
		{
			name:       "live uses superfast",
			live:       true,
			wantPreset: "superfast",
			invoke: func(service *playback.HLSService, request playback.HLSRequest) error {
				_, err := service.Live(t.Context(), request)
				return err
			},
			args: func(manager *hlsFakeManager) []string { return manager.lastStart.Args },
		},
		{
			name:       "VOD segment remains veryfast",
			wantPreset: "veryfast",
			invoke: func(service *playback.HLSService, request playback.HLSRequest) error {
				_, err := service.Segment(t.Context(), request)
				return err
			},
			args: func(manager *hlsFakeManager) []string { return manager.lastRestart.Args },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{playlist: "#EXTM3U\n", segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
			request := hlsTranscodeRequest()
			request.Live = tc.live

			if err := tc.invoke(service, request); err != nil {
				t.Fatalf("HLS operation: %v", err)
			}
			if !hlsArgPair(tc.args(manager), "-preset", tc.wantPreset) {
				t.Errorf("HLS argv = %q, want service-projected %s transcode preset", tc.args(manager), tc.wantPreset)
			}
		})
	}
}

func TestHLSServiceSegmentBuildsLowBandwidthH264AC3JobAndDelegatesLifecycle(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	manager := &hlsFakeManager{segment: "segment four"}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.SegmentIndex = 4

	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	if got := readHLSResponse(t, response); got != "segment four" {
		t.Errorf("Segment() body = %q, want manager segment bytes", got)
	}
	if manager.startCalls != 0 || manager.restartCalls != 1 || manager.waitSegmentCalls != 1 || manager.lastWaitIndex != 4 {
		t.Fatalf("Segment() lifecycle calls = start:%d restart:%d wait:%d index:%d, want one manager-owned restart and wait without a redundant start", manager.startCalls, manager.restartCalls, manager.waitSegmentCalls, manager.lastWaitIndex)
	}
	if manager.pingCalls != 1 || manager.lastPing != request.PlaySessionID {
		t.Errorf("Segment() pings = %d play session %q, want active request session %q", manager.pingCalls, manager.lastPing, request.PlaySessionID)
	}
	if manager.lastRestart.PlaySessionID != request.PlaySessionID || manager.lastRestart.DeviceID != request.DeviceID {
		t.Errorf("HLS job IDs = %#v, want play session %q and device %q", manager.lastRestart, request.PlaySessionID, request.DeviceID)
	}
	if manager.lastRestart.PlaylistPath != response.OutputPath {
		t.Errorf("HLS job playlist path = %q, want response output path %q", manager.lastRestart.PlaylistPath, response.OutputPath)
	}
	wantPattern := strings.TrimSuffix(response.OutputPath, ".m3u8") + "%d.ts"
	if manager.lastRestart.SegmentPattern != wantPattern {
		t.Errorf("HLS TS segment pattern = %q, want response-family pattern %q", manager.lastRestart.SegmentPattern, wantPattern)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-codec:v:0", "libx264") || !hlsArgPair(manager.lastRestart.Args, "-codec:a:0", "aac") {
		t.Errorf("HLS argv = %q, want caller-resolved LowBandwidth H264/AC3 transcode through x264 and AAC", manager.lastRestart.Args)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-hls_segment_filename", manager.lastRestart.SegmentPattern) {
		t.Errorf("HLS argv = %q, want explicit manager segment pattern %q", manager.lastRestart.Args, manager.lastRestart.SegmentPattern)
	}
}

func TestHLSServiceSegmentRebuildsRestartArgsFromRequestedRuntimeAndIndex(t *testing.T) {
	for _, tc := range []struct {
		name         string
		runtimeTicks int64
		segmentIndex int
		wantSeek     string
		wantNoSeek   bool
	}{
		{name: "cold initial segment", segmentIndex: 0, wantNoSeek: true},
		{name: "backward seek", runtimeTicks: 900_000_000, segmentIndex: 9, wantSeek: "00:01:30.000"},
		{name: "forward gap", runtimeTicks: 1_700_000_000, segmentIndex: 17, wantSeek: "00:02:50.000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
			request := hlsTranscodeRequest()
			request.RuntimeTicks = tc.runtimeTicks
			request.SegmentIndex = tc.segmentIndex

			if _, err := service.Segment(t.Context(), request); err != nil {
				t.Fatalf("Segment(): %v", err)
			}
			if !hlsArgPair(manager.lastRestart.Args, "-start_number", itoa(tc.segmentIndex)) {
				t.Errorf("restart argv = %q, want start number %d", manager.lastRestart.Args, tc.segmentIndex)
			}
			if tc.wantNoSeek {
				if hlsHasArg(manager.lastRestart.Args, "-ss") {
					t.Errorf("cold restart argv = %q, must not seek when RuntimeTicks is zero", manager.lastRestart.Args)
				}
				return
			}
			if !hlsArgPair(manager.lastRestart.Args, "-ss", tc.wantSeek) {
				t.Errorf("restart argv = %q, want source-shaped seek %q from RuntimeTicks", manager.lastRestart.Args, tc.wantSeek)
			}
		})
	}
}

// Segment requests of one rendition differ only in their seek and start
// number. The manager joins a running job by IdentityArgs, so that identity
// must not depend on the segment.
func TestHLSServiceSegmentIdentityIsIndependentOfRequestedSegment(t *testing.T) {
	manager := &hlsFakeManager{segment: "segment"}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
	var identities [][]string
	for _, segment := range []struct {
		runtimeTicks int64
		index        int
	}{{0, 0}, {130_130_000, 1}, {900_000_000, 9}} {
		request := hlsTranscodeRequest()
		request.RuntimeTicks = segment.runtimeTicks
		request.SegmentIndex = segment.index
		if _, err := service.Segment(t.Context(), request); err != nil {
			t.Fatalf("Segment(%d): %v", segment.index, err)
		}
		job := manager.lastRestart
		if len(job.IdentityArgs) == 0 {
			t.Fatalf("segment %d manager job has no identity argv", segment.index)
		}
		if hlsHasArg(job.IdentityArgs, "-ss") || !hlsArgPair(job.IdentityArgs, "-start_number", "0") {
			t.Errorf("segment %d identity argv = %q, want no seek and start number 0", segment.index, job.IdentityArgs)
		}
		if !hlsArgPair(job.Args, "-start_number", itoa(segment.index)) {
			t.Errorf("segment %d launch argv = %q, want its own start number", segment.index, job.Args)
		}
		identities = append(identities, job.IdentityArgs)
	}
	for index := range identities[1:] {
		if !reflect.DeepEqual(identities[0], identities[index+1]) {
			t.Errorf("identity argv differs between segments:\n%q\n%q", identities[0], identities[index+1])
		}
	}
}

func TestHLSServiceProjectsMediaSourceAndStartTimeToManagerJobs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		live           bool
		startTimeTicks int64
		runtimeTicks   int64
		segmentIndex   int
		wantStartTicks int64
		wantSeek       string
	}{
		{name: "initial segment", segmentIndex: 0},
		{name: "live job", live: true, startTimeTicks: 900_000_000, wantStartTicks: 900_000_000, wantSeek: "00:01:30.000"},
		{name: "seek restart", runtimeTicks: 900_000_000, segmentIndex: 9, wantStartTicks: 900_000_000, wantSeek: "00:01:30.000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{playlist: "#EXTM3U\n", segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
			request := hlsTranscodeRequest()
			request.Live = tc.live
			request.StartTimeTicks = tc.startTimeTicks
			request.RuntimeTicks = tc.runtimeTicks
			request.SegmentIndex = tc.segmentIndex

			var err error
			if tc.live {
				_, err = service.Live(t.Context(), request)
			} else {
				_, err = service.Segment(t.Context(), request)
			}
			if err != nil {
				t.Fatalf("HLS operation: %v", err)
			}
			job := manager.lastRestart
			if tc.live {
				job = manager.lastStart
			}
			if job.MediaSourceID != request.MediaSourceID || job.StartTimeTicks != tc.wantStartTicks {
				t.Errorf("manager job = %#v, want media source %q and start time %d", job, request.MediaSourceID, tc.wantStartTicks)
			}
			if tc.wantSeek != "" && !hlsArgPair(job.Args, "-ss", tc.wantSeek) {
				t.Errorf("manager argv = %q, want start seek %q", job.Args, tc.wantSeek)
			}
		})
	}
}

func TestHLSServiceSegmentUsesRestartWithoutStartForRetainedSuccessfulJob(t *testing.T) {
	manager := &hlsFakeManager{
		startErr: errors.New("retained HLS job must not be started again"),
		segment:  "retained segment",
	}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
	request := hlsTranscodeRequest()
	request.SegmentIndex = 8

	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(retained job): %v", err)
	}
	if got := readHLSResponse(t, response); got != "retained segment" {
		t.Errorf("Segment(retained job) body = %q, want manager retained segment", got)
	}
	if manager.startCalls != 0 || manager.restartCalls != 1 || manager.waitSegmentCalls != 1 {
		t.Errorf("retained Segment() calls = start:%d restart:%d wait:%d, want restart then wait without StartHLS", manager.startCalls, manager.restartCalls, manager.waitSegmentCalls)
	}
}

func TestHLSServiceCopiesEligibleAACAudioWithoutReencoding(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/aac-copy.mp4")
	audio := &sourceInfo.MediaStreams[1]
	audio.Codec = "aac"
	bitrate, channels := 128_000, 2
	audio.BitRate, audio.Channels = &bitrate, &channels
	manager := &hlsFakeManager{segment: "AAC segment"}
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)
	request := hlsTranscodeRequest()
	request.AudioCodec, request.AudioCodecs = "aac", []string{"aac"}
	request.AllowAudioStreamCopy, request.EnableAutoStreamCopy = true, true

	if _, err := service.Segment(t.Context(), request); err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-codec:a:0", "copy") {
		t.Fatalf("AAC-compatible HLS argv = %q, want stream copy rather than AAC reencode", manager.lastRestart.Args)
	}
	if hlsArgPair(manager.lastRestart.Args, "-codec:a:0", "aac") || hlsHasArg(manager.lastRestart.Args, "-ab") || hlsHasArg(manager.lastRestart.Args, "-ac") {
		t.Errorf("AAC-compatible HLS argv = %q, must not project reencode-only audio options", manager.lastRestart.Args)
	}
}

// TestHLSServiceCopyAudioDoesNotAlterVideoBitrateBudget asserts that when
// stock copies audio instead of encoding it, the video bitrate budget
// (-maxrate) does not change. The measurement on the pinned stock image
// (12.0-rc2) showed identical -maxrate values for the copy and encode legs at
// the same total bitrate. hlsEncodingJob passes request.VideoBitrate through
// hlsOutputBitrate unconditionally, so the copy decision must not adjust
// OutputVideoBitrate.
func TestHLSServiceCopyAudioDoesNotAlterVideoBitrateBudget(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/aac-copy-budget.mp4")
	audio := &sourceInfo.MediaStreams[1]
	audio.Codec = "aac"
	audioBitrate, channels := 96_000, 2
	audio.BitRate, audio.Channels = &audioBitrate, &channels

	for _, tc := range []struct {
		name         string
		allowCopy    bool
		wantAudioArg string
	}{
		{name: "copy", allowCopy: true, wantAudioArg: "copy"},
		{name: "encode", allowCopy: false, wantAudioArg: "aac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)
			request := hlsTranscodeRequest()
			request.VideoBitrate = hlsInt(500_000)
			request.AudioCodec, request.AudioCodecs = "aac", []string{"aac"}
			request.AllowAudioStreamCopy = tc.allowCopy
			request.EnableAutoStreamCopy = tc.allowCopy
			request.AudioBitrate = hlsInt(96_000)

			if _, err := service.Segment(t.Context(), request); err != nil {
				t.Fatalf("Segment(): %v", err)
			}
			if !hlsArgPair(manager.lastRestart.Args, "-codec:a:0", tc.wantAudioArg) {
				t.Fatalf("audio argv = %q, want -codec:a:0 %q", manager.lastRestart.Args, tc.wantAudioArg)
			}
			// The -maxrate must equal the requested VideoBitrate in both the
			// copy and the encode case. The stock measurement showed no
			// difference between the two legs.
			wantMaxrate := strconv.Itoa(500_000)
			if !hlsArgPair(manager.lastRestart.Args, "-maxrate", wantMaxrate) {
				t.Errorf("-maxrate argv = %q, want %q (request.VideoBitrate); audio %s must not alter the video budget", manager.lastRestart.Args, wantMaxrate, tc.wantAudioArg)
			}
		})
	}
}

func TestHLSServiceProjectsRequestedAudioSampleRateIntoHLSArgs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sampleRate *int
		wantRate   bool
	}{
		{name: "positive request", sampleRate: hlsInt(48_000), wantRate: true},
		{name: "absent request", sampleRate: nil, wantRate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)
			request := hlsTranscodeRequest()
			request.SegmentContainer = "fmp4"
			request.AudioSampleRate = tc.sampleRate

			if _, err := service.Segment(t.Context(), request); err != nil {
				t.Fatalf("Segment(): %v", err)
			}
			if tc.wantRate {
				if !hlsArgSequence(manager.lastRestart.Args,
					"-codec:a:0", "aac",
					"-ac", "2",
					"-ab", "128000",
					"-ar", "48000",
					"-copyts", "-avoid_negative_ts", "disabled",
				) {
					t.Errorf("HLS argv = %q, want requested audio sample rate after canonical codec/channel/bitrate options and before later audio/timestamp options", manager.lastRestart.Args)
				}
				return
			}
			if hlsArgPair(manager.lastRestart.Args, "-ar", "48000") {
				t.Errorf("HLS argv = %q, must omit audio sample-rate option when request has none", manager.lastRestart.Args)
			}
		})
	}
}

func TestHLSServiceDelegatesSegmentRestartDecisionForBackwardAndGapSeeks(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	manager := &hlsFakeManager{segment: "segment"}
	service := newHLSService(source, manager)

	for _, index := range []int{10, 9, 17} { // forward, backward, then gap above 24/4.
		request := hlsTranscodeRequest()
		request.SegmentLengthSeconds = 4
		request.SegmentIndex = index
		if _, err := service.Segment(t.Context(), request); err != nil {
			t.Fatalf("Segment(%d): %v", index, err)
		}
	}
	if got, want := manager.restartIndexes, []int{10, 9, 17}; !sameHLSIndexes(got, want) {
		t.Errorf("RestartHLSAt indexes = %#v, want requested forward/backward/gap indexes %#v for manager lifecycle policy", got, want)
	}
}

func TestHLSServiceFMP4SegmentJobUsesMP4PatternAndInitMinusOne(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	manager := &hlsFakeManager{segment: "init"}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.SegmentContainer = "fmp4"
	request.SegmentIndex = -1

	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(fMP4 init): %v", err)
	}
	if response.ContentType != "video/mp4" {
		t.Errorf("fMP4 Segment().ContentType = %q, want video/mp4", response.ContentType)
	}
	wantPattern := strings.TrimSuffix(response.OutputPath, ".m3u8") + "%d.mp4"
	initName := strings.TrimSuffix(strings.TrimPrefix(response.OutputPath, "/cache/"), ".m3u8") + "-1.mp4"
	if manager.lastRestart.SegmentPattern != wantPattern {
		t.Errorf("HLS fMP4 segment pattern = %q, want response-family pattern %q", manager.lastRestart.SegmentPattern, wantPattern)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-hls_fmp4_init_filename", initName) {
		t.Errorf("HLS fMP4 argv = %q, want init fragment %q", manager.lastRestart.Args, initName)
	}
	if !hlsArgSequence(manager.lastRestart.Args,
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", initName,
		"-start_number", "0",
		"-hls_segment_filename", manager.lastRestart.SegmentPattern,
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_segment_options", "movflags=+frag_discont",
		"-y", response.OutputPath,
	) {
		t.Errorf("HLS fMP4 argv = %q, want exact canonical init/start/segment/playlist/list/options/output order", manager.lastRestart.Args)
	}
	if !sameHLSIndexes(manager.restartIndexes, []int{-1}) {
		t.Errorf("RestartHLSAt indexes = %#v, want init segment -1", manager.restartIndexes)
	}
}

func TestHLSServiceNormalizesPublicMP4SegmentsToInternalFMP4(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/goldenrec-input.mp4")
	runtime := int64(130_000_000)
	sourceInfo.RunTimeTicks = &runtime
	source := &progressiveFakeSource{source: sourceInfo}
	manager := &hlsFakeManager{segment: "init"}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.SegmentContainer = "mp4"
	request.SegmentLengthSeconds = 6
	request.EndpointPrefix = "hls/main/"
	request.RawQuery = "api_key=token-3"

	mainResponse, err := service.Main(t.Context(), request)
	if err != nil {
		t.Fatalf("Main(public mp4): %v", err)
	}
	mainPlaylist := readHLSResponse(t, mainResponse)
	for _, fragment := range []string{
		"#EXT-X-VERSION:7\n",
		"#EXT-X-MAP:URI=\"hls/main/-1.mp4?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=0\"",
		"hls/main/0.mp4?api_key=token-3&runtimeTicks=0&actualSegmentLengthTicks=60000000",
	} {
		if !strings.Contains(mainPlaylist, fragment) {
			t.Errorf("Main(public mp4) playlist = %q, want public fMP4 URI fragment %q", mainPlaylist, fragment)
		}
	}

	request.SegmentIndex = -1
	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(public mp4 init): %v", err)
	}
	if response.ContentType != "video/mp4" {
		t.Errorf("Segment(public mp4).ContentType = %q, want video/mp4", response.ContentType)
	}
	wantPattern := strings.TrimSuffix(response.OutputPath, ".m3u8") + "%d.mp4"
	initName := strings.TrimSuffix(strings.TrimPrefix(response.OutputPath, "/cache/"), ".m3u8") + "-1.mp4"
	if manager.lastRestart.SegmentPattern != wantPattern {
		t.Errorf("public mp4 HLS pattern = %q, want response-family pattern %q", manager.lastRestart.SegmentPattern, wantPattern)
	}
	if !hlsArgPair(manager.lastRestart.Args, "-hls_segment_type", "fmp4") || !hlsArgPair(manager.lastRestart.Args, "-hls_fmp4_init_filename", initName) {
		t.Errorf("public mp4 HLS argv = %q, want internal fMP4 segment type and init fragment %q", manager.lastRestart.Args, initName)
	}
}

func TestHLSServiceHeadBuildsMetadataWithoutStartingOrReadingTranscode(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	manager := &hlsFakeManager{segment: "must not open", playlist: "must not read"}
	service := newHLSService(source, manager)
	request := hlsTranscodeRequest()
	request.Head = true
	request.SegmentContainer = "fmp4"

	response, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("Segment(HEAD): %v", err)
	}
	if response.Body != nil {
		t.Error("Segment(HEAD).Body is non-nil, want no manager-owned stream")
	}
	if !strings.HasPrefix(response.OutputPath, "/cache/") || !strings.HasSuffix(response.OutputPath, ".m3u8") || response.ContentType != "video/mp4" {
		t.Errorf("Segment(HEAD) metadata = %#v, want validated fMP4 rendition metadata", response)
	}
	if source.calls != 1 || manager.startCalls != 0 || manager.restartCalls != 0 || manager.waitSegmentCalls != 0 || manager.waitPlaylistCalls != 0 {
		t.Errorf("Segment(HEAD) calls = source:%d start:%d restart:%d segment:%d playlist:%d, want source validation only", source.calls, manager.startCalls, manager.restartCalls, manager.waitSegmentCalls, manager.waitPlaylistCalls)
	}
}

func TestHLSServiceStopDelegatesSessionAndDeviceCorrelation(t *testing.T) {
	manager := &hlsFakeManager{}
	service := newHLSService(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, manager)

	if err := service.Stop(t.Context(), "device-7", "session-9"); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if manager.stopCalls != 1 || manager.stopDeviceID != "device-7" || manager.stopPlaySessionID != "session-9" {
		t.Errorf("Stop() manager call = calls:%d device:%q session:%q, want exact correlation", manager.stopCalls, manager.stopDeviceID, manager.stopPlaySessionID)
	}
}

func TestHLSServiceCanonicalizesSelectedSourceIDAcrossVideoRenditions(t *testing.T) {
	const selectedSourceID = "00000000-0000-0000-0000-000000000704"
	for _, tc := range []struct {
		name            string
		mediaSourceID   string
		rawQuery        string
		wantSourceInput string
	}{
		{
			name:            "omitted media source ID",
			rawQuery:        "ApiKey=token-3",
			wantSourceInput: "",
		},
		{
			name:            "accepted item ID alias",
			mediaSourceID:   "00000000-0000-0000-0000-000000000702",
			rawQuery:        "MediaSourceId=00000000-0000-0000-0000-000000000702&ApiKey=token-3",
			wantSourceInput: "00000000-0000-0000-0000-000000000702",
		},
		{
			name:            "explicit selected source ID",
			mediaSourceID:   selectedSourceID,
			rawQuery:        "MediaSourceId=" + selectedSourceID + "&ApiKey=token-3",
			wantSourceInput: selectedSourceID,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/canonical-source.mkv")
			sourceInfo.ID = selectedSourceID
			runtime := int64(350_000_000)
			sourceInfo.RunTimeTicks = &runtime
			language := "eng"
			sourceInfo.MediaStreams = append(sourceInfo.MediaStreams, playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "srt", Language: &language})
			source := &progressiveFakeSource{source: sourceInfo}
			manager := &hlsFakeManager{segment: "segment"}
			keyframeSource := &hlsFakeKeyframes{}
			trickplays := &hlsFakeTrickplays{metadata: []playback.HLSTrickplayMetadata{{Width: 320, Height: 180, Bandwidth: 64_000}}}
			config := hlsServiceConfig(source, manager)
			config.Keyframes = keyframeSource
			config.Trickplays = trickplays
			service := playback.NewHLSService(config)

			request := hlsTranscodeRequest()
			request.MediaSourceID = tc.mediaSourceID
			request.RawQuery = tc.rawQuery
			request.APIKey = "token-3"
			request.EnableTrickplay = true
			request.EnableSubtitlesInManifest = true
			request.AllowVideoStreamCopy = true
			request.VideoBitrate = hlsInt(2_600_000)

			masterResponse, err := service.Master(t.Context(), request)
			if err != nil {
				t.Fatalf("Master(): %v", err)
			}
			master := readHLSResponse(t, masterResponse)
			if got := hlsURIQueryValue(t, master, "main.m3u8?", "MediaSourceId"); got != selectedSourceID {
				t.Errorf("Master() main rendition MediaSourceId = %q, want selected source ID %q", got, selectedSourceID)
			}
			if want := "URI=\"" + selectedSourceID + "/Subtitles/2/subtitles.m3u8?SegmentLength=30&ApiKey=token-3\""; !strings.Contains(master, want) {
				t.Errorf("Master() subtitle rendition = %q, want URI fragment %q", master, want)
			}
			if want := "URI=\"Trickplay/320/tiles.m3u8?MediaSourceId=" + selectedSourceID + "&ApiKey=token-3\""; !strings.Contains(master, want) {
				t.Errorf("Master() trickplay rendition = %q, want URI fragment %q", master, want)
			}
			if wantSourceID := progressiveUUID(selectedSourceID); trickplays.calls != 1 || trickplays.sourceID != wantSourceID {
				t.Errorf("TrickplayMetadata call = calls:%d source:%s, want selected source ID %s", trickplays.calls, trickplays.sourceID, selectedSourceID)
			}

			mainResponse, err := service.Main(t.Context(), request)
			if err != nil {
				t.Fatalf("Main(): %v", err)
			}
			main := readHLSResponse(t, mainResponse)
			if got := hlsURIQueryValue(t, main, "hls/main/0.ts?", "MediaSourceId"); got != selectedSourceID {
				t.Errorf("Main() segment rendition MediaSourceId = %q, want selected source ID %q", got, selectedSourceID)
			}
			if keyframeSource.calls != 1 || keyframeSource.mediaSourceID != selectedSourceID {
				t.Errorf("Keyframes call = calls:%d source:%q, want selected source ID %q", keyframeSource.calls, keyframeSource.mediaSourceID, selectedSourceID)
			}

			if _, err := service.Segment(t.Context(), request); err != nil {
				t.Fatalf("Segment(): %v", err)
			}
			if manager.restartCalls != 1 || manager.lastRestart.MediaSourceID != selectedSourceID {
				t.Errorf("RestartHLSAt call = calls:%d source:%q, want selected source ID %q", manager.restartCalls, manager.lastRestart.MediaSourceID, selectedSourceID)
			}
			if source.calls != 3 || source.mediaSourceID != tc.wantSourceInput {
				t.Errorf("StaticSource calls = %d last source input %q, want three calls retaining accepted request identity %q", source.calls, source.mediaSourceID, tc.wantSourceInput)
			}
		})
	}
}

func hlsURIQueryValue(t *testing.T, playlist, prefix, key string) string {
	t.Helper()
	for line := range strings.SplitSeq(playlist, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		uri, err := url.Parse(line)
		if err != nil {
			t.Fatalf("parse generated rendition URI %q: %v", line, err)
		}
		return uri.Query().Get(key)
	}
	t.Fatalf("playlist = %q, want rendition URI starting with %q", playlist, prefix)
	return ""
}

func hlsURIWithPrefix(t *testing.T, playlist, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(playlist, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("playlist = %q, want URI beginning with %q", playlist, prefix)
	return ""
}

func hlsAttributeURI(t *testing.T, playlist, linePrefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(playlist, "\n") {
		if !strings.HasPrefix(line, linePrefix) {
			continue
		}
		const marker = `URI="`
		start := strings.Index(line, marker)
		if start < 0 {
			t.Fatalf("playlist line = %q, want URI attribute", line)
		}
		value := line[start+len(marker):]
		end := strings.IndexByte(value, '"')
		if end < 0 {
			t.Fatalf("playlist line = %q, has unterminated URI attribute", line)
		}
		return value[:end]
	}
	t.Fatalf("playlist = %q, want line beginning with %q", playlist, linePrefix)
	return ""
}

func TestHLSServiceMainPassesConfiguredKeyframesForEligibleRemux(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/feature.mkv")
	runtime := int64(350_000_000)
	sourceInfo.RunTimeTicks = &runtime
	request := hlsTranscodeRequest()
	sourceInfo.ID = request.MediaSourceID
	request.AllowVideoStreamCopy = true
	request.VideoBitrate = hlsInt(2_600_000)
	request.RawQuery = "api_key=token-3"
	request.SegmentLengthSeconds = 6
	keyframeSource := &hlsFakeKeyframes{
		data: keyframes.Data{
			TotalDurationTicks: runtime,
			KeyframeTicks:      []int64{0, 104_270_000, 208_540_000, 312_400_000},
		},
		found: true,
	}
	config := hlsServiceConfig(&progressiveFakeSource{source: sourceInfo}, &hlsFakeManager{})
	config.Keyframes = keyframeSource
	service := playback.NewHLSService(config)

	response, err := service.Main(t.Context(), request)
	if err != nil {
		t.Fatalf("Main(): %v", err)
	}
	if keyframeSource.calls != 1 || keyframeSource.itemID != request.ItemID || keyframeSource.mediaSourceID != request.MediaSourceID {
		t.Fatalf("Keyframes call = calls:%d item:%s source:%q, want requested remux source facts", keyframeSource.calls, keyframeSource.itemID, keyframeSource.mediaSourceID)
	}
	playlist := readHLSResponse(t, response)
	for _, fragment := range []string{
		"#EXT-X-TARGETDURATION:11\n",
		"#EXTINF:10.427000, nodesc\n",
		"hls/main/0.ts?api_key=token-3&MediaSourceId=" + request.MediaSourceID + "&runtimeTicks=0&actualSegmentLengthTicks=104270000",
		"hls/main/3.ts?api_key=token-3&MediaSourceId=" + request.MediaSourceID + "&runtimeTicks=312400000&actualSegmentLengthTicks=37600000",
	} {
		if !strings.Contains(playlist, fragment) {
			t.Errorf("keyframe-snapped Main() playlist = %q, want fragment %q", playlist, fragment)
		}
	}
}

func TestHLSServiceMasterEmitsTrickplayOnlyForEnabledNonLiveRequest(t *testing.T) {
	for _, tc := range []struct {
		name, apiKey        string
		enabled, live, want bool
	}{
		{name: "enabled VOD", apiKey: "token-3", enabled: true, want: true},
		{name: "disabled VOD", apiKey: "token-3", enabled: false, want: false},
		{name: "enabled live", apiKey: "token-3", enabled: true, live: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := hlsTranscodeRequest()
			request.EnableTrickplay = tc.enabled
			request.Live = tc.live
			request.APIKey = tc.apiKey
			trickplays := &hlsFakeTrickplays{metadata: []playback.HLSTrickplayMetadata{{Width: 320, Height: 180, Bandwidth: 64_000}}}
			config := hlsServiceConfig(&progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, &hlsFakeManager{})
			config.Trickplays = trickplays
			service := playback.NewHLSService(config)

			response, err := service.Master(t.Context(), request)
			if err != nil {
				t.Fatalf("Master(): %v", err)
			}
			playlist := readHLSResponse(t, response)
			imageLine := "#EXT-X-IMAGE-STREAM-INF:BANDWIDTH=64000,RESOLUTION=320x180,CODECS=\"jpeg\",URI=\"Trickplay/320/tiles.m3u8?MediaSourceId=" + request.MediaSourceID + "&ApiKey=token-3\""
			if got := strings.Contains(playlist, imageLine); got != tc.want {
				t.Errorf("Master() trickplay line present = %t, want %t; playlist = %q", got, tc.want, playlist)
			}
			if tc.want {
				wantSourceID := progressiveUUID("00000000-0000-0000-0000-000000000703")
				if trickplays.calls != 1 || trickplays.sourceID != wantSourceID {
					t.Errorf("TrickplayMetadata call = calls:%d source:%s, want one requested media-source ID %s", trickplays.calls, trickplays.sourceID, wantSourceID)
				}
				return
			}
			if trickplays.calls != 0 {
				t.Errorf("TrickplayMetadata calls = %d, want none when master request is disabled or live", trickplays.calls)
			}
		})
	}
}

func hlsServiceConfig(source playback.DirectFileSourceService, manager playback.HLSManager) playback.HLSServiceConfig {
	return playback.HLSServiceConfig{
		Sources:      source,
		TranscodeDir: "/cache",
		Capabilities: encoding.Capabilities{
			H264Encoder:      "libx264",
			AACEncoders:      []string{"aac"},
			SupportsTonemapX: true,
		},
		Manager: manager,
	}
}

func newHLSService(source playback.DirectFileSourceService, manager playback.HLSManager) *playback.HLSService {
	return playback.NewHLSService(hlsServiceConfig(source, manager))
}

func hlsTranscodeRequest() playback.HLSRequest {
	request := playback.HLSRequest{
		ProgressiveRequest: playback.ProgressiveRequest{
			UserID:               progressiveUUID("00000000-0000-0000-0000-000000000701"),
			ItemID:               progressiveUUID("00000000-0000-0000-0000-000000000702"),
			MediaSourceID:        "00000000-0000-0000-0000-000000000703",
			DeviceID:             "device-7",
			PlaySessionID:        "session-9",
			UserAgent:            "DeadfinTest/1.0",
			VideoCodec:           "h264",
			VideoCodecs:          []string{"h264"},
			AudioCodec:           "aac",
			AudioCodecs:          []string{"aac"},
			VideoBitrate:         hlsInt(1_000_000),
			AudioBitrate:         hlsInt(128_000),
			MaxWidth:             hlsInt(1280),
			MaxHeight:            hlsInt(720),
			MaxAudioChannels:     hlsInt(2),
			AllowVideoStreamCopy: false,
			AllowAudioStreamCopy: false,
			EnableAutoStreamCopy: false,
			CopyTimestamps:       true,
		},
		SegmentContainer:     "ts",
		SegmentLengthSeconds: 6,
		EndpointPrefix:       "hls/main/",
	}
	return request
}

func hlsInt(value int) *int { return &value }

func readHLSResponse(t *testing.T, response playback.HLSResponse) string {
	t.Helper()
	if response.Body == nil {
		t.Fatal("HLS response Body = nil")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read HLS response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close HLS response: %v", err)
	}
	return string(body)
}

func hlsArgPair(args []string, option, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == option && args[index+1] == value {
			return true
		}
	}
	return false
}

func hlsHasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func hlsArgSequence(args []string, want ...string) bool {
	if len(want) == 0 || len(args) < len(want) {
		return false
	}
	for start := 0; start+len(want) <= len(args); start++ {
		matched := true
		for offset := range want {
			if args[start+offset] != want[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func sameHLSIndexes(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

type hlsFakeKeyframes struct {
	data          keyframes.Data
	found         bool
	err           error
	calls         int
	itemID        dto.UUID
	mediaSourceID string
}

func (source *hlsFakeKeyframes) Keyframes(_ context.Context, itemID dto.UUID, mediaSourceID string) (keyframes.Data, bool, error) {
	source.calls++
	source.itemID, source.mediaSourceID = itemID, mediaSourceID
	return source.data, source.found, source.err
}

type hlsFakeTrickplays struct {
	metadata []playback.HLSTrickplayMetadata
	err      error
	calls    int
	sourceID dto.UUID
}

func (source *hlsFakeTrickplays) TrickplayMetadata(_ context.Context, sourceID dto.UUID) ([]playback.HLSTrickplayMetadata, error) {
	source.calls++
	source.sourceID = sourceID
	return source.metadata, source.err
}

type hlsFakeManager struct {
	startErr, restartErr, waitErr, stopErr error
	segment, playlist                      string
	openSource                             func(playback.HLSJobRequest) error

	startCalls, restartCalls, waitSegmentCalls, waitPlaylistCalls, pingCalls, stopCalls int
	openCalls                                                                           int
	lastStart, lastRestart                                                              playback.HLSJobRequest
	restartIndexes                                                                      []int
	lastWaitIndex                                                                       int
	waitPlaylistPath                                                                    string
	waitPlaylistMinSegments                                                             int
	lastPing                                                                            string
	stopPlaySessionID, stopDeviceID                                                     string
}

func (manager *hlsFakeManager) StartHLS(_ context.Context, request playback.HLSJobRequest) error {
	manager.startCalls++
	manager.lastStart = request
	if manager.openSource != nil {
		manager.openCalls++
		if err := manager.openSource(request); err != nil {
			return err
		}
	}
	return manager.startErr
}

func (manager *hlsFakeManager) RestartHLSAt(_ context.Context, request playback.HLSJobRequest, index int) error {
	manager.restartCalls++
	manager.lastRestart = request
	manager.restartIndexes = append(manager.restartIndexes, index)
	if manager.openSource != nil {
		manager.openCalls++
		if err := manager.openSource(request); err != nil {
			return err
		}
	}
	return manager.restartErr
}

func (manager *hlsFakeManager) WaitSegment(_ context.Context, _ string, index int, _ *playback.HLSSegmentTiming) (io.ReadCloser, error) {
	manager.waitSegmentCalls++
	manager.lastWaitIndex = index
	if manager.waitErr != nil {
		return nil, manager.waitErr
	}
	return io.NopCloser(strings.NewReader(manager.segment)), nil
}

func (manager *hlsFakeManager) WaitPlaylist(_ context.Context, playlistPath string, minSegments int) (string, error) {
	manager.waitPlaylistCalls++
	manager.waitPlaylistPath = playlistPath
	manager.waitPlaylistMinSegments = minSegments
	if manager.waitErr != nil {
		return "", manager.waitErr
	}
	return manager.playlist, nil
}

func (manager *hlsFakeManager) PingPlaySession(playSessionID string) bool {
	manager.pingCalls++
	manager.lastPing = playSessionID
	return true
}

func (manager *hlsFakeManager) StopPlaySession(_ context.Context, playSessionID, deviceID string) error {
	manager.stopCalls++
	manager.stopPlaySessionID, manager.stopDeviceID = playSessionID, deviceID
	return manager.stopErr
}

func TestHLSServiceBurnsSelectedEmbeddedNonFirstTextSubtitle(t *testing.T) {
	source := progressiveCopySource("/media/it's a: film,dir/feature.mkv")
	language := "eng"
	source.MediaStreams = append(source.MediaStreams,
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "srt", Language: &language},
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 3, Codec: "ass", Language: &language},
	)
	manager := &hlsFakeManager{segment: "encoded segment"}
	service := newHLSService(&progressiveFakeSource{source: source}, manager)
	request := hlsTranscodeRequest()
	selected := 3
	delivery := playback.SubtitleEncode
	request.SubtitleStreamIndex = &selected
	request.SubtitleDeliveryMethod = &delivery

	if _, err := service.Segment(t.Context(), request); err != nil {
		t.Fatalf("Segment() with selected embedded text subtitle: %v", err)
	}
	token := manager.lastRestart.EmbeddedSubtitleSource
	filter := hlsConstraintArgumentValue(manager.lastRestart.Args, "-vf")
	if token == "" || strings.Count(filter, token) != 1 || !strings.HasPrefix(token, "subtitles=f='") || !strings.HasSuffix(token, "'") {
		t.Fatalf("Segment() embedded subtitle token/filter = %q/%q, want one exact escaped subtitles operand", token, filter)
	}
	// si= counts subtitle streams only: stream 3 is the second subtitle.
	if !strings.Contains(filter, token+":si=1") {
		t.Fatalf("Segment() embedded subtitle filter = %q, want token for the second subtitle stream", filter)
	}
	if strings.Contains(strings.Join(manager.lastRestart.Args, "\x00"), "/dev/fd/") {
		t.Fatalf("Segment() canonical args unexpectedly contain runtime descriptor paths: %q", manager.lastRestart.Args)
	}

	liveManager := &hlsFakeManager{playlist: "#EXTM3U\n"}
	liveService := newHLSService(&progressiveFakeSource{source: source}, liveManager)
	liveRequest := request
	liveRequest.Live = true
	response, err := liveService.Live(t.Context(), liveRequest)
	if err != nil {
		t.Fatalf("Live() with selected embedded text subtitle: %v", err)
	}
	_ = readHLSResponse(t, response)
	if liveManager.lastStart.EmbeddedSubtitleSource != token {
		t.Fatalf("Live() embedded subtitle token = %q, want exact segment token %q", liveManager.lastStart.EmbeddedSubtitleSource, token)
	}
}

func TestHLSServiceBurnsSelectedEmbeddedDVDSubtitle(t *testing.T) {
	source := progressiveCopySource("/media/dazed-and-confused.mkv")
	source.MediaStreams = append(source.MediaStreams,
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "dvdsub"},
	)
	manager := &hlsFakeManager{segment: "encoded segment"}
	service := newHLSService(&progressiveFakeSource{source: source}, manager)
	request := hlsTranscodeRequest()
	selected := 2
	delivery := playback.SubtitleEncode
	request.SubtitleStreamIndex = &selected
	request.SubtitleDeliveryMethod = &delivery

	if _, err := service.Segment(t.Context(), request); err != nil {
		t.Fatalf("Segment() with selected embedded dvdsub subtitle: %v", err)
	}
	if manager.restartCalls == 0 {
		t.Fatal("Segment() did not call the HLS manager for an embedded dvdsub burn-in")
	}
	args := manager.lastRestart.Args
	if args == nil {
		t.Fatal("Segment() manager request has no args for an embedded dvdsub burn-in")
	}
	graph := hlsConstraintArgumentValue(args, "-filter_complex")
	if graph == "" {
		t.Fatalf("Segment() embedded dvdsub argv has no -filter_complex graph: %q", args)
	}
	if !strings.HasPrefix(graph, "[0:2]") {
		t.Fatalf("Segment() embedded dvdsub overlay graph = %q, want it to read input 0 stream 2 first", graph)
	}
	if !strings.HasSuffix(graph, "overlay=eof_action=pass:repeatlast=0") {
		t.Fatalf("Segment() embedded dvdsub overlay graph = %q, want overlay=eof_action=pass:repeatlast=0 suffix", graph)
	}
	for _, arg := range args {
		if strings.Contains(arg, "subtitles=") {
			t.Fatalf("Segment() embedded dvdsub argv sent the bitmap stream through a subtitles= text filter: %q", arg)
		}
	}
	if manager.lastRestart.EmbeddedSubtitleSource != "" {
		t.Fatalf("Segment() embedded dvdsub EmbeddedSubtitleSource = %q, want empty for a bitmap-overlay burn-in", manager.lastRestart.EmbeddedSubtitleSource)
	}
}

func TestHLSServiceOmitsEmbeddedSubtitleSourceUnlessEligible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*playback.MediaSourceInfo, *playback.HLSRequest)
	}{
		{name: "graphical subtitle burn", mutate: func(source *playback.MediaSourceInfo, _ *playback.HLSRequest) {
			source.MediaStreams[2].Codec = "pgssub"
		}},
		{name: "video copy", mutate: func(_ *playback.MediaSourceInfo, request *playback.HLSRequest) {
			request.AllowVideoStreamCopy, request.AllowAudioStreamCopy, request.EnableAutoStreamCopy = true, true, true
			request.VideoBitrate, request.AudioBitrate, request.MaxAudioChannels = hlsInt(2_600_000), hlsInt(384_000), hlsInt(6)
			request.AudioCodec, request.AudioCodecs = "ac3", []string{"ac3"}
			delivery := playback.SubtitleHLS
			request.SubtitleDeliveryMethod = &delivery
		}},
		{name: "audio only", mutate: func(_ *playback.MediaSourceInfo, request *playback.HLSRequest) {
			request.AudioOnly = true
		}},
		{name: "subtitle not burned", mutate: func(_ *playback.MediaSourceInfo, request *playback.HLSRequest) {
			delivery := playback.SubtitleHLS
			request.SubtitleDeliveryMethod = &delivery
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := progressiveCopySource("/media/eligible-check.mkv")
			source.MediaStreams = append(source.MediaStreams, playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 5, Codec: "srt"})
			request := hlsTranscodeRequest()
			selected, delivery := 5, playback.SubtitleEncode
			request.SubtitleStreamIndex, request.SubtitleDeliveryMethod = &selected, &delivery
			tc.mutate(&source, &request)
			manager := &hlsFakeManager{segment: "segment"}
			service := newHLSService(&progressiveFakeSource{source: source}, manager)
			if _, err := service.Segment(t.Context(), request); err != nil {
				t.Fatalf("Segment(): %v", err)
			}
			if got := manager.lastRestart.EmbeddedSubtitleSource; got != "" {
				t.Fatalf("EmbeddedSubtitleSource = %q, want empty for %s", got, tc.name)
			}
		})
	}
}

func TestHLSServicePinsExternalSubtitleAndAuthorizesItsRendition(t *testing.T) {
	subtitlePath := filepath.Join(t.TempDir(), "external.srt")
	const original = "original external subtitle"
	if err := os.WriteFile(subtitlePath, []byte(original), 0o600); err != nil {
		t.Fatalf("write external subtitle: %v", err)
	}
	source := progressiveCopySource("/media/external-subtitle-feature.mkv")
	source.MediaStreams = append(source.MediaStreams, playback.MediaStream{
		Type: playback.StreamTypeSubtitle, Index: 5, Codec: "srt", IsExternal: true, Path: subtitlePath,
	})
	manager := &hlsFakeManager{segment: "segment"}
	service := newHLSService(&progressiveFakeSource{source: source}, manager)
	request := hlsTranscodeRequest()
	selected, delivery := 5, playback.SubtitleEncode
	request.SubtitleStreamIndex, request.SubtitleDeliveryMethod = &selected, &delivery

	first, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("first Segment(): %v", err)
	}
	authority := manager.lastRestart
	if authority.EmbeddedSubtitleSource != "" || authority.ExternalSubtitleSource == "" ||
		authority.ExternalSubtitleRevision == "" || authority.OpenExternalSubtitle == nil {
		t.Fatalf("external subtitle authority = %#v, want exact token, revision, and opener", authority)
	}
	pinned, err := authority.OpenExternalSubtitle()
	if err != nil {
		t.Fatalf("open authorized external subtitle: %v", err)
	}
	got, readErr := io.ReadAll(pinned.File())
	closeErr := pinned.Close()
	if readErr != nil || closeErr != nil || string(got) != original {
		t.Fatalf("authorized subtitle bytes/read/close = %q/%v/%v", got, readErr, closeErr)
	}

	if err := os.WriteFile(subtitlePath, []byte("changed external subtitle"), 0o600); err != nil {
		t.Fatalf("replace external subtitle: %v", err)
	}
	if stale, openErr := authority.OpenExternalSubtitle(); openErr == nil {
		_ = stale.Close()
		t.Fatal("old external subtitle authority opened changed bytes")
	} else if !errors.Is(openErr, sourcefile.ErrStaleSource) {
		t.Fatalf("old external subtitle opener error = %v, want ErrStaleSource", openErr)
	}
	second, err := service.Segment(t.Context(), request)
	if err != nil {
		t.Fatalf("replacement Segment(): %v", err)
	}
	if first.OutputPath == second.OutputPath {
		t.Fatalf("external subtitle replacement reused rendition family %q", first.OutputPath)
	}
}

func TestHLSServiceDerivesDistinctRenditionFamiliesForChangedEffectiveJobs(t *testing.T) {
	source := progressiveCopySource("/media/same-source.mkv")
	audioBitrate, audioChannels := 128_000, 2
	source.MediaStreams = append(source.MediaStreams,
		playback.MediaStream{Type: playback.StreamTypeAudio, Index: 3, Codec: "ac3", BitRate: &audioBitrate, Channels: &audioChannels},
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 5, Codec: "srt"},
	)
	service := newHLSService(&progressiveFakeSource{source: source}, &hlsFakeManager{})
	base := hlsTranscodeRequest()
	base.MaxWidth, base.MaxHeight = hlsInt(1280), hlsInt(720)
	base.AudioCodec, base.AudioCodecs = "ac3", []string{"ac3"}
	base.MaxAudioChannels = hlsInt(6)
	base.AudioBitrate = hlsInt(384_000)
	base.AllowAudioStreamCopy, base.EnableAutoStreamCopy = true, true

	output := func(t *testing.T, request playback.HLSRequest) string {
		t.Helper()
		response, err := service.Master(t.Context(), request)
		if err != nil {
			t.Fatalf("Master(): %v", err)
		}
		_ = readHLSResponse(t, response)
		return response.OutputPath
	}
	baseOutput := output(t, base)
	if got := output(t, base); got != baseOutput {
		t.Fatalf("identical HLS request output = %q, want deduplicated family %q", got, baseOutput)
	}

	for _, test := range []struct {
		name   string
		mutate func(*playback.HLSRequest)
	}{
		{
			name: "selected audio stream",
			mutate: func(request *playback.HLSRequest) {
				selected := 3
				request.AudioStreamIndex = &selected
			},
		},
		{
			name: "selected burned subtitle",
			mutate: func(request *playback.HLSRequest) {
				selected := 5
				delivery := playback.SubtitleEncode
				request.SubtitleStreamIndex, request.SubtitleDeliveryMethod = &selected, &delivery
			},
		},
		{
			name: "requested audio codec",
			mutate: func(request *playback.HLSRequest) {
				request.AudioCodec, request.AudioCodecs = "aac", []string{"aac"}
			},
		},
		{
			name: "requested H264 profile",
			mutate: func(request *playback.HLSRequest) {
				profile := "Main"
				request.Profile = &profile
			},
		},
		{
			name: "requested video bitrate",
			mutate: func(request *playback.HLSRequest) {
				request.VideoBitrate = hlsInt(800_000)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			if got := output(t, request); got == baseOutput {
				t.Fatalf("changed effective HLS job reused output family %q", got)
			}
		})
	}
}

func TestHLSServiceCanonicalSourceRevisionSeparatesSameSessionRenditions(t *testing.T) {
	sourceInfo := progressiveCopySource("/media/canonical-source-revision.mp4")
	sourceInfo.ID = hlsTranscodeRequest().MediaSourceID
	inode := int64(7_001)
	sourceInfo.FileFacts.Inode = &inode
	changedAt := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	sourceInfo.FileFacts.ChangedAt = &changedAt
	sourceInfo.SourceRevision = "canonical-revision-a"
	source := &progressiveFakeSource{source: sourceInfo}
	service := newHLSService(source, &hlsFakeManager{})
	request := hlsTranscodeRequest()

	first, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("first Master(): %v", err)
	}
	_ = readHLSResponse(t, first)

	source.source.SourceRevision = "canonical-revision-b"
	second, err := service.Master(t.Context(), request)
	if err != nil {
		t.Fatalf("replacement Master(): %v", err)
	}
	_ = readHLSResponse(t, second)

	if second.OutputPath == first.OutputPath {
		t.Fatalf("distinct canonical source revisions reused rendition identity %q", second.OutputPath)
	}
}

func TestHLSServiceUsesCollisionFreeSessionAndDeviceIdentity(t *testing.T) {
	service := newHLSService(
		&progressiveFakeSource{source: progressiveCopySource("/media/session-key-feature.mp4")},
		&hlsFakeManager{},
	)
	first := hlsTranscodeRequest()
	first.PlaySessionID, first.DeviceID = "a\x00b", "c"
	second := first
	second.PlaySessionID, second.DeviceID = "a", "b\x00c"
	firstResponse, err := service.Master(t.Context(), first)
	if err != nil {
		t.Fatalf("first Master(): %v", err)
	}
	secondResponse, err := service.Master(t.Context(), second)
	if err != nil {
		t.Fatalf("second Master(): %v", err)
	}
	if firstResponse.OutputPath == secondResponse.OutputPath {
		t.Fatalf("NUL-delimited collision candidates shared rendition %q", firstResponse.OutputPath)
	}
}

func TestHLSServiceRejectsIncompleteCanonicalSourceAuthority(t *testing.T) {
	request := hlsTranscodeRequest()
	canonical := progressiveCopySource("/media/incomplete-source-identity.mp4")
	canonical.ID = request.MediaSourceID

	for _, tc := range []struct {
		name   string
		mutate func(*playback.MediaSourceInfo)
	}{
		{
			name: "inode only",
			mutate: func(source *playback.MediaSourceInfo) {
				source.FileFacts.ChangedAt = nil
			},
		},
		{
			name: "changed at only",
			mutate: func(source *playback.MediaSourceInfo) {
				source.FileFacts.Inode = nil
			},
		},
		{
			name: "empty revision with complete facts",
			mutate: func(source *playback.MediaSourceInfo) {
				source.SourceRevision = ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			incomplete := canonical
			tc.mutate(&incomplete)
			service := newHLSService(&progressiveFakeSource{source: incomplete}, &hlsFakeManager{})
			if _, err := service.Master(t.Context(), request); !errors.Is(err, playback.ErrProgressiveUnsupported) {
				t.Fatalf("Master() error = %v, want ErrProgressiveUnsupported", err)
			}
		})
	}
}

func TestHLSServiceSegmentAndLiveProjectSourceRevisionAndOpenPinnedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.mp4")
	const contents = "pinned HLS source bytes"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sourceInfo := hlsSourceWithSnapshot(t, path, "canonical-source-revision")

	for _, tc := range []struct {
		name string
		run  func(context.Context, *playback.HLSService, playback.HLSRequest) (playback.HLSResponse, error)
	}{
		{name: "segment", run: func(ctx context.Context, service *playback.HLSService, request playback.HLSRequest) (playback.HLSResponse, error) {
			return service.Segment(ctx, request)
		}},
		{name: "live", run: func(ctx context.Context, service *playback.HLSService, request playback.HLSRequest) (playback.HLSResponse, error) {
			return service.Live(ctx, request)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened string
			manager := &hlsFakeManager{segment: "segment", playlist: "#EXTM3U\n"}
			manager.openSource = func(request playback.HLSJobRequest) error {
				if request.SourceRevision != sourceInfo.SourceRevision {
					t.Errorf("manager SourceRevision = %q, want canonical %q", request.SourceRevision, sourceInfo.SourceRevision)
				}
				var err error
				opened, err = hlsReadPinnedSource(request)
				return err
			}
			service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)

			response, err := tc.run(t.Context(), service, hlsTranscodeRequest())
			if err != nil {
				t.Fatalf("%s(): %v", tc.name, err)
			}
			_ = readHLSResponse(t, response)
			if manager.openCalls != 1 {
				t.Fatalf("%s() source opens = %d, want one manager-owned pinned open", tc.name, manager.openCalls)
			}
			if opened != contents {
				t.Fatalf("%s() pinned source bytes = %q, want %q", tc.name, opened, contents)
			}
		})
	}
}

func TestHLSServiceMasterAndMainNeverOpenSource(t *testing.T) {
	inode := int64(7_003)
	sourceInfo := progressiveCopySource(filepath.Join(t.TempDir(), "missing-source.mp4"))
	sourceInfo.ID = hlsTranscodeRequest().MediaSourceID
	sourceInfo.SourceRevision = "canonical-but-not-opened"
	sourceInfo.FileFacts.Inode = &inode
	runtimeTicks := int64(60_000_000)
	sourceInfo.RunTimeTicks = &runtimeTicks

	for _, tc := range []struct {
		name string
		run  func(context.Context, *playback.HLSService, playback.HLSRequest) (playback.HLSResponse, error)
	}{
		{name: "master", run: func(ctx context.Context, service *playback.HLSService, request playback.HLSRequest) (playback.HLSResponse, error) {
			return service.Master(ctx, request)
		}},
		{name: "main", run: func(ctx context.Context, service *playback.HLSService, request playback.HLSRequest) (playback.HLSResponse, error) {
			return service.Main(ctx, request)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &hlsFakeManager{openSource: func(playback.HLSJobRequest) error {
				t.Error("playlist-only endpoint unexpectedly reached source-opening manager")
				return errors.New("unexpected source open")
			}}
			service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)

			response, err := tc.run(t.Context(), service, hlsTranscodeRequest())
			if err != nil {
				t.Fatalf("%s(): %v", tc.name, err)
			}
			_ = readHLSResponse(t, response)
			if manager.openCalls != 0 || manager.startCalls != 0 || manager.restartCalls != 0 {
				t.Fatalf("%s() manager calls = opens:%d starts:%d restarts:%d, want no source or lifecycle access", tc.name, manager.openCalls, manager.startCalls, manager.restartCalls)
			}
		})
	}
}

func TestHLSServiceSegmentOpenSourceValidatesEverySnapshotFact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*playback.MediaSourceInfo)
	}{
		{
			name: "size",
			mutate: func(source *playback.MediaSourceInfo) {
				wrong := *source.FileFacts.Size + 1
				source.FileFacts.Size = &wrong
			},
		},
		{
			name: "modified at",
			mutate: func(source *playback.MediaSourceInfo) {
				wrong := source.FileFacts.ModifiedAt.Add(time.Second)
				source.FileFacts.ModifiedAt = &wrong
			},
		},
		{
			name: "inode",
			mutate: func(source *playback.MediaSourceInfo) {
				wrong := *source.FileFacts.Inode + 1
				source.FileFacts.Inode = &wrong
			},
		},
		{
			name: "changed at",
			mutate: func(source *playback.MediaSourceInfo) {
				wrong := source.FileFacts.ChangedAt.Add(time.Second)
				source.FileFacts.ChangedAt = &wrong
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.mp4")
			if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
				t.Fatalf("write source: %v", err)
			}
			sourceInfo := hlsSourceWithSnapshot(t, path, "canonical-source-revision")
			tc.mutate(&sourceInfo)
			manager := &hlsFakeManager{openSource: func(request playback.HLSJobRequest) error {
				_, err := hlsReadPinnedSource(request)
				return err
			}}
			service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)

			_, err := service.Segment(t.Context(), hlsTranscodeRequest())
			if !errors.Is(err, sourcefile.ErrStaleSource) {
				t.Fatalf("Segment() with stale %s snapshot error = %v, want sourcefile.ErrStaleSource", tc.name, err)
			}
		})
	}
}

func TestHLSServiceSegmentRejectsReplacementBeforeManagerUsesSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.mp4")
	const original = "original-source-bytes"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write original source: %v", err)
	}
	sourceInfo := hlsSourceWithSnapshot(t, path, "canonical-source-revision")
	replacementPath := filepath.Join(dir, "replacement.mp4")
	if err := os.WriteFile(replacementPath, []byte(strings.Repeat("R", len(original))), 0o600); err != nil {
		t.Fatalf("write replacement source: %v", err)
	}
	if err := os.Chtimes(replacementPath, *sourceInfo.FileFacts.ModifiedAt, *sourceInfo.FileFacts.ModifiedAt); err != nil {
		t.Fatalf("preserve replacement mtime: %v", err)
	}
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatalf("replace source atomically: %v", err)
	}

	manager := &hlsFakeManager{openSource: func(request playback.HLSJobRequest) error {
		_, err := hlsReadPinnedSource(request)
		return err
	}}
	service := newHLSService(&progressiveFakeSource{source: sourceInfo}, manager)
	_, err := service.Segment(t.Context(), hlsTranscodeRequest())
	if !errors.Is(err, sourcefile.ErrStaleSource) {
		t.Fatalf("Segment() after source replacement error = %v, want sourcefile.ErrStaleSource", err)
	}
}

func hlsSourceWithSnapshot(t *testing.T, path, revision string) playback.MediaSourceInfo {
	t.Helper()
	pinned, err := sourcefile.Open(path, sourcefile.Expected{})
	if err != nil {
		t.Fatalf("open source snapshot: %v", err)
	}
	fingerprint := pinned.Fingerprint()
	if err := pinned.Close(); err != nil {
		t.Fatalf("close source snapshot: %v", err)
	}
	source := progressiveCopySource(path)
	source.ID = hlsTranscodeRequest().MediaSourceID
	source.SourceRevision = revision
	source.FileFacts = playback.SourceFileFacts{
		Size:       &fingerprint.Size,
		ModifiedAt: &fingerprint.MTime,
		Inode:      hlsInt64(int64(fingerprint.Ino)),
		ChangedAt:  &fingerprint.CTime,
	}
	return source
}

func hlsReadPinnedSource(request playback.HLSJobRequest) (string, error) {
	if request.OpenSource == nil {
		return "", errors.New("HLS manager request has no source opener")
	}
	pinned, err := request.OpenSource()
	if err != nil {
		return "", err
	}
	contents, readErr := io.ReadAll(pinned.File())
	closeErr := pinned.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return string(contents), nil
}

func hlsInt64(value int64) *int64 { return &value }

// TestHLSServiceAppliesEncodingHardwareOptions proves the encoding.xml
// options reach the ffmpeg argv: a QSV-configured server decodes an HDR10
// HEVC source with VAAPI, tone maps with VPP, and encodes with h264_qsv.
func TestHLSServiceAppliesEncodingHardwareOptions(t *testing.T) {
	source := progressiveCopySource("/media/1917.mkv")
	transfer, pixelFormat, profile, bitDepth := "smpte2084", "yuv420p10le", "Main 10", 10
	width, height := 3840, 2160
	video := &source.MediaStreams[0]
	video.Codec = "hevc"
	video.ColorTransfer = &transfer
	video.PixelFormat = &pixelFormat
	video.Profile = &profile
	video.BitDepth = &bitDepth
	video.Width, video.Height = &width, &height

	manager := &hlsFakeManager{segment: "encoded segment"}
	config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
	config.Capabilities.SupportsTonemapX = false
	config.Capabilities.EncoderMajorVersion, config.Capabilities.EncoderMinorVersion, config.Capabilities.EncoderPatchVersion = 8, 1, 1
	config.Capabilities.Encoders = []string{"h264_qsv", "libx264"}
	config.Capabilities.HWAccels = []string{"opencl", "qsv", "vaapi"}
	config.Capabilities.Filters = []string{"alphasrc", "overlay_opencl", "scale_opencl", "tonemap_opencl", "tonemap_vaapi"}
	config.Hardware = func() encoding.HardwareOptions {
		return encoding.HardwareOptions{
			Type:                           encoding.HardwareAccelerationQSV,
			EnableHardwareEncoding:         true,
			HardwareDecodingCodecs:         []string{"h264", "hevc"},
			EnableDecodingColorDepth10Hevc: true,
			PreferSystemNativeHwDecoder:    true,
			EnableTonemapping:              true,
			EnableVppTonemapping:           true,
		}
	}
	service := playback.NewHLSService(config)

	if _, err := service.Segment(t.Context(), hlsTranscodeRequest()); err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	args := manager.lastRestart.Args
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-init_hw_device vaapi=va:,vendor_id=0x8086,driver=iHD -init_hw_device qsv=qs@va -init_hw_device opencl=ocl@va -filter_hw_device qs -hwaccel vaapi -hwaccel_output_format vaapi -noautorotate -i file:/media/1917.mkv -noautoscale",
		"-codec:v:0 h264_qsv",
		"tonemap_vaapi=format=nv12:p=bt709:t=bt709:m=bt709:extra_hw_frames=32,hwmap=derive_device=qsv,format=qsv",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Segment() argv = %q, want %q", joined, want)
		}
	}
}

// Two play sessions on one item get separate playlist families even on one
// DeviceId; one session on two clients shares its family, as upstream
// GetOutputFilePath keys by DeviceId and PlaySessionId.
func TestHLSServiceKeysConcurrentSessionsOnOneItemApart(t *testing.T) {
	service := newHLSService(
		&progressiveFakeSource{source: progressiveCopySource("/media/two-televisions.mkv")},
		&hlsFakeManager{},
	)
	outputPath := func(playSessionID, deviceID string) string {
		t.Helper()
		request := hlsTranscodeRequest()
		request.PlaySessionID, request.DeviceID = playSessionID, deviceID
		response, err := service.Master(t.Context(), request)
		if err != nil {
			t.Fatalf("Master(%q, %q): %v", playSessionID, deviceID, err)
		}
		return response.OutputPath
	}
	livingRoom := outputPath("play-session-a", "atv-living-room")
	if got := outputPath("play-session-b", "atv-bedroom"); got == livingRoom {
		t.Fatalf("two devices share playlist family %q", got)
	}
	if got := outputPath("play-session-b", "atv-living-room"); got == livingRoom {
		t.Fatalf("two play sessions on one device share playlist family %q", got)
	}
	if got := outputPath("play-session-a", "atv-living-room"); got != livingRoom {
		t.Fatalf("one play session moved to playlist family %q, want %q", got, livingRoom)
	}
}

// A copy of a source without probed dimensions cannot write an fMP4 track
// header, and the client may have sent no bitrate to transcode with. The
// segment container falls back to mpegts, which needs neither.
func TestHLSServiceLiveSizelessFMP4Copy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		codec       string
		wantCodec   string
		wantSegment string
	}{
		// H.264 copies into mpegts segments, which carry no frame size.
		{name: "h264 copies into mpegts", codec: "h264", wantCodec: "copy", wantSegment: "mpegts"},
		// Apple HLS plays HEVC only in fMP4, so HEVC transcodes and the
		// encoder supplies the size.
		{name: "hevc transcodes into fMP4", codec: "hevc", wantSegment: "fmp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioBitrate, channels := 128_000, 2
			source := playback.MediaSourceInfo{
				Protocol:     playback.MediaProtocolHTTP,
				Path:         "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/stream-1/stream.ts",
				Container:    "mpegts",
				LiveStreamID: "live-1",
				MediaStreams: []playback.MediaStream{
					{Type: playback.StreamTypeVideo, Index: 0, Codec: tc.codec},
					{Type: playback.StreamTypeAudio, Index: 1, Codec: "aac", BitRate: &audioBitrate, Channels: &channels, IsDefault: true},
				},
			}
			manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
			config := hlsServiceConfig(&progressiveFakeSource{}, manager)
			config.LiveTv = hlsLiveTvStub{source: source}
			service := playback.NewHLSService(config)
			request := hlsTranscodeRequest()
			request.Live = true
			request.LiveStreamID = "live-1"
			request.MaxWidth, request.MaxHeight, request.VideoBitrate = nil, nil, nil
			// The Apple TV clients list HEVC first, then H.264.
			request.VideoCodec, request.VideoCodecs = tc.codec, []string{tc.codec, "h264"}
			request.AllowVideoStreamCopy = true
			request.SegmentContainer = "fmp4"
			request.AudioStreamIndex = hlsInt(1)

			if _, err := service.Live(t.Context(), request); err != nil {
				t.Fatalf("Live() sizeless %s: %v", tc.codec, err)
			}
			copied := hlsArgPair(manager.lastStart.Args, "-codec:v:0", "copy")
			if copied != (tc.wantCodec == "copy") {
				t.Errorf("Live() argv = %q, video copy = %v, want %v", manager.lastStart.Args, copied, tc.wantCodec == "copy")
			}
			if !hlsArgPair(manager.lastStart.Args, "-hls_segment_type", tc.wantSegment) {
				t.Errorf("Live() argv = %q, want %s segments", manager.lastStart.Args, tc.wantSegment)
			}
		})
	}
}

// Without any bitrate the tuner fallback supplies -maxrate. Without that
// fallback too, the transcode still builds and runs on CRF alone.
func TestHLSServiceLiveBitrateWithoutARate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback *int
		wantRate string
	}{
		{name: "tuner fallback", fallback: hlsInt(8_000_000), wantRate: "8000000"},
		{name: "no rate at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audioBitrate, channels := 128_000, 2
			source := playback.MediaSourceInfo{
				Protocol:                    playback.MediaProtocolHTTP,
				Path:                        "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/stream-1/stream.ts",
				Container:                   "mpegts",
				LiveStreamID:                "live-1",
				FallbackMaxStreamingBitrate: tc.fallback,
				MediaStreams: []playback.MediaStream{
					{Type: playback.StreamTypeVideo, Index: 0, Codec: "mpeg2video"},
					{Type: playback.StreamTypeAudio, Index: 1, Codec: "aac", BitRate: &audioBitrate, Channels: &channels, IsDefault: true},
				},
			}
			manager := &hlsFakeManager{playlist: "#EXTM3U\n"}
			config := hlsServiceConfig(&progressiveFakeSource{}, manager)
			config.LiveTv = hlsLiveTvStub{source: source}
			service := playback.NewHLSService(config)
			request := hlsTranscodeRequest()
			request.Live = true
			request.LiveStreamID = "live-1"
			request.MaxWidth, request.MaxHeight, request.VideoBitrate = nil, nil, nil
			request.AudioStreamIndex = hlsInt(1)

			if _, err := service.Live(t.Context(), request); err != nil {
				t.Fatalf("Live() without a bitrate: %v", err)
			}
			args := manager.lastStart.Args
			if tc.wantRate == "" {
				if hlsArgPair(args, "-maxrate", "0") {
					t.Errorf("argv = %q, want no -maxrate 0", args)
				}
				return
			}
			if !hlsArgPair(args, "-maxrate", tc.wantRate) {
				t.Errorf("argv = %q, want -maxrate %s from the tuner fallback", args, tc.wantRate)
			}
		})
	}
}
