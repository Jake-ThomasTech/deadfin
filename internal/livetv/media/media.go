// SPDX-License-Identifier: GPL-2.0-only

// Package media maps Live TV channels to playback media sources and opens
// them through the stream broker. It ports LiveTvMediaSourceProvider and
// the M3U half of CreateMediaSourceInfo (docs/spikes/livetv-upstream.md
// §2.5, §2.8).
//
// Every source path a client sees is the broker URL
// /LiveTv/LiveStreamFiles/{id}/stream.ts or empty; the tuner's channel URL,
// which may carry provider credentials, stays on the server (§4.3,
// operator decision 9).
package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/stream"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/deadfin-org/deadfin/internal/playback"
)

const (
	// providerName and serviceName are the upstream type names whose md5
	// prefixes OpenToken and LiveStreamId (MediaSourceManager.SetKeyProperties,
	// LiveTvMediaSourceProvider.GetChannelStream).
	providerName = "Jellyfin.LiveTv.LiveTvMediaSourceProvider"
	serviceName  = "Jellyfin.LiveTv.DefaultLiveTvService"
	// channelTypeName is LiveTvChannel's GetType().Name in the OpenToken.
	channelTypeName = "LiveTvChannel"
	// defaultBufferMs is Normalize's BufferMs default.
	defaultBufferMs = 1500
	// analyzeDurationMs is what LiveStreamHelper sets after the probe wait.
	analyzeDurationMs = 3000
	// probeDelay is LiveStreamHelper's wait before the probe:
	// max(3000, AnalyzeDurationMs), and an unopened source has none.
	probeDelay = 3 * time.Second
)

// Channels resolves and opens channel streams; livetv.Service implements it.
type Channels interface {
	ChannelStream(ctx context.Context, itemID dto.UUID) (livetv.ChannelStream, bool, error)
	OpenStream(ctx context.Context, channel livetv.ChannelStream) (io.ReadCloser, error)
}

// Options configures a Provider.
type Options struct {
	Channels Channels
	Broker   *stream.Broker
	// LocalURL returns the server's local base URL, for example
	// "http://192.168.1.5:8096", without a trailing slash. The broker path
	// is appended to it (upstream GetLocalApiUrl).
	LocalURL func() string
	// IsLocalHost reports a stream host inside the local network
	// (INetworkManager.IsInLocalNetwork). Nil treats every host as remote.
	IsLocalHost func(host string) bool
	// NewKey returns a key for an unshared open. Nil uses a counter.
	NewKey func() string
	// Probe reads the media info of an opened stream from its broker URL
	// (MediaEncoder.GetMediaInfo). Nil keeps the unprobed source.
	Probe func(ctx context.Context, url string) (playback.MediaSourceInfo, error)
	// Clock times the wait before the probe. Nil uses the real clock.
	Clock clock.Clock
	// Logger records probe failures. Nil uses slog.Default.
	Logger *slog.Logger
	// ReleaseDevice releases the live streams a device plays for which
	// release reports true (session.Manager.ReleaseDeviceLiveStreams). Nil
	// releases nothing.
	ReleaseDevice func(ctx context.Context, deviceID string, release func(liveStreamID string) bool)
	// ReopenDevice reports that a device opened a live stream, so the
	// session registry tracks it again (session.Manager.ReopenDeviceLiveStream).
	// Nil does nothing.
	ReopenDevice func(deviceID, liveStreamID string)
}

// Provider implements playback.LiveTvSources and playback.LiveStreamProvider.
type Provider struct {
	options Options

	mu     sync.Mutex
	opened map[string]openStream
	next   uint64
}

// openStream is the source of an open broker stream and the startup times
// of its first open.
type openStream struct {
	source  playback.MediaSourceInfo
	startup playback.LiveStartup
}

// New returns a Provider.
func New(options Options) (*Provider, error) {
	if options.Channels == nil || options.Broker == nil || options.LocalURL == nil {
		return nil, errors.New("livetv media: channels, broker and local url are required")
	}
	if options.Clock == nil {
		options.Clock = clock.NewReal()
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Provider{options: options, opened: make(map[string]openStream)}, nil
}

// ChannelSources implements playback.LiveTvSources. It returns the one
// source of a channel's tuner, before opening (CreateMediaSourceInfo,
// Normalize, OpenToken).
func (p *Provider) ChannelSources(ctx context.Context, itemID dto.UUID) ([]playback.MediaSourceInfo, bool, error) {
	channel, ok, err := p.options.Channels.ChannelStream(ctx, itemID)
	if !ok || err != nil {
		if errors.Is(err, livetv.ErrNotFound) {
			return []playback.MediaSourceInfo{}, true, nil
		}
		return nil, ok, err
	}
	source := p.channelSource(channel)
	source.OpenToken = prefix(providerName) + strings.Join([]string{channelTypeName, itemID.String(), source.ID}, "_")
	return []playback.MediaSourceInfo{source}, true, nil
}

// channelSource ports M3UTunerHost.CreateMediaSourceInfo
// (M3UTunerHost.cs:144-211) and LiveTvMediaSourceProvider.Normalize, with
// no path: the channel URL stays on the server.
func (p *Provider) channelSource(channel livetv.ChannelStream) playback.MediaSourceInfo {
	info := channel.Tuner
	isRemote := true
	if parsed, err := url.Parse(channel.Channel.Path); err == nil && p.options.IsLocalHost != nil {
		isRemote = !p.options.IsLocalHost(parsed.Hostname())
	}
	bufferMs := defaultBufferMs
	source := playback.MediaSourceInfo{
		ID:       tuner.DotNetMD5(channel.Channel.Path),
		Protocol: playback.MediaProtocolHTTP,
		Type:     playback.MediaSourceDefault,
		MediaStreams: []playback.MediaStream{
			{Type: playback.StreamTypeVideo, Index: -1, IsInterlaced: true},
			{Type: playback.StreamTypeAudio, Index: -1},
		},
		MediaAttachments:                    []playback.MediaAttachment{},
		Formats:                             []string{},
		RequiresOpening:                     true,
		RequiresClosing:                     true,
		RequiresLooping:                     info.EnableStreamLooping,
		ReadAtNativeFramerate:               info.ReadAtNativeFramerate,
		IsInfiniteStream:                    true,
		IsRemote:                            isRemote,
		IgnoreDts:                           info.IgnoreDts,
		SupportsDirectPlay:                  !info.EnableStreamLooping && info.TunerCount == 0,
		SupportsDirectStream:                !info.EnableStreamLooping,
		SupportsTranscoding:                 true,
		SupportsProbing:                     true,
		UseMostCompatibleTranscodingProfile: !info.AllowFmp4TranscodingContainer,
		BufferMs:                            &bufferMs,
		RequiredHTTPHeaders:                 map[string]string{},
	}
	if channel.Item.Extra.LiveTV != nil && channel.Item.Extra.LiveTV.ChannelType == item.ChannelTypeRadio {
		source.MediaStreams = source.MediaStreams[1:]
	}
	if info.FallbackMaxStreamingBitrate > 0 {
		value := info.FallbackMaxStreamingBitrate
		source.FallbackMaxStreamingBitrate = &value
	}
	return source
}

// prefix is md5(name) + "_", the upstream id prefix.
func prefix(name string) string { return tuner.DotNetMD5(name) + "_" }

// OpenLiveStream implements playback.LiveStreamProvider. It opens the
// channel named by the OpenToken through the broker, shared with other
// viewers when the tuner allows stream sharing.
func (p *Provider) OpenLiveStream(ctx context.Context, request playback.OpenLiveStreamRequest) (playback.LiveStreamResponse, error) {
	received := p.options.Clock.Now()
	itemID, err := tokenItemID(request.OpenToken)
	if err != nil {
		return playback.LiveStreamResponse{}, err
	}
	channel, ok, err := p.options.Channels.ChannelStream(ctx, itemID)
	if err != nil {
		return playback.LiveStreamResponse{}, err
	}
	if !ok {
		return playback.LiveStreamResponse{}, fmt.Errorf("%w: not a live tv channel", playback.ErrItemNotFound)
	}
	key := channel.Tuner.Id + "\x00" + channel.Channel.ID
	if !channel.Tuner.AllowStreamSharing {
		key += "\x00" + p.newKey()
	}
	p.releaseSwitched(ctx, request.DeviceID, channel.Tuner.Id, key)
	info, err := p.options.Broker.Open(ctx, stream.Source{
		Key:         key,
		TunerHostID: channel.Tuner.Id,
		TunerCount:  channel.Tuner.TunerCount,
		// Rapid channel switching connects to the provider at most once
		// per interval (D-93).
		ConnectInterval: tuner.StreamOpenInterval(channel.Tuner),
		Connect: func(connectCtx context.Context) (io.ReadCloser, error) {
			return p.options.Channels.OpenStream(connectCtx, channel)
		},
	})
	if errors.Is(err, stream.ErrTunerBusy) {
		return playback.LiveStreamResponse{}, fmt.Errorf("%w: %w", playback.ErrLiveStreamBusy, err)
	}
	if err != nil {
		return playback.LiveStreamResponse{}, err
	}

	source := p.channelSource(channel)
	analyze := analyzeDurationMs
	source.LiveStreamID = prefix(providerName) + prefix(serviceName) + info.ID
	source.Path = p.options.LocalURL() + "/LiveTv/LiveStreamFiles/" + info.ID + "/stream.ts"
	source.Protocol = playback.MediaProtocolHTTP
	source.AnalyzeDurationMs = &analyze
	// A broker stream is read through the server; the client sees no
	// provider headers.
	source.RequiredHTTPHeaders = map[string]string{}

	p.mu.Lock()
	p.sweep()
	shared, joined := p.opened[info.ID]
	p.mu.Unlock()
	p.reopened(request.DeviceID, source.LiveStreamID)
	if joined {
		// A viewer that joins a shared stream gets the probed source of
		// the first open (upstream reads the OpenToken probe cache).
		return playback.LiveStreamResponse{MediaSource: shared.source}, nil
	}
	if err := p.probe(ctx, &source); err != nil {
		p.options.Broker.Release(info.ID)
		return playback.LiveStreamResponse{}, err
	}

	startup := playback.LiveStartup{Received: received, Connected: info.Connected, FirstByte: info.FirstByte, Probed: p.options.Clock.Now()}

	p.mu.Lock()
	p.opened[info.ID] = openStream{source: source, startup: startup}
	p.mu.Unlock()
	return playback.LiveStreamResponse{MediaSource: source}, nil
}

// releaseSwitched releases, before a device opens a stream, the device's
// streams on the same tuner other than the one it opens (D-92). A client
// that switches channels reports the stop of the old channel only after
// the new one plays, so the old stream would hold a tuner slot that the
// new open needs.
func (p *Provider) releaseSwitched(ctx context.Context, deviceID, tunerHostID, key string) {
	if deviceID == "" || p.options.ReleaseDevice == nil {
		return
	}
	p.options.ReleaseDevice(ctx, deviceID, func(liveStreamID string) bool {
		info, open := p.options.Broker.Info(streamID(liveStreamID))
		return open && info.TunerHostID == tunerHostID && info.Key != key
	})
}

// reopened reports a device's open of a stream that a switch may have
// released earlier (D-92).
func (p *Provider) reopened(deviceID, liveStreamID string) {
	if deviceID != "" && p.options.ReopenDevice != nil {
		p.options.ReopenDevice(deviceID, liveStreamID)
	}
}

// probe ports LiveStreamHelper.AddMediaInfoWithProbe for a first open: it
// waits for the stream to fill, probes the broker URL and copies the media
// info into source. A probe failure keeps the unprobed source, as upstream
// logs and continues (MediaSourceManager.OpenLiveStreamInternal). Only a
// cancelled context is an error.
func (p *Provider) probe(ctx context.Context, source *playback.MediaSourceInfo) error {
	if p.options.Probe == nil || !source.SupportsProbing {
		return nil
	}
	timer := p.options.Clock.NewTimer(probeDelay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C():
	}
	info, err := p.options.Probe(ctx, source.Path)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		p.options.Logger.Warn("live tv: probe of the opened stream failed", "liveStreamId", source.LiveStreamID, "error", err)
		return nil
	}
	applyProbe(source, info)
	return nil
}

// applyProbe copies probed media info into an opened source
// (LiveStreamHelper.cs:124-182). The probed stream indexes stay, so the
// encoder maps real streams (divergence D-101).
func applyProbe(source *playback.MediaSourceInfo, info playback.MediaSourceInfo) {
	source.Bitrate = info.Bitrate
	source.Container = info.Container
	source.Formats = info.Formats
	if source.Formats == nil {
		source.Formats = []string{}
	}
	source.MediaStreams = info.MediaStreams
	// A live source has no run time, so it plays as a live stream.
	source.RunTimeTicks = nil
	source.Size = info.Size
	source.Timestamp = info.Timestamp
	source.Video3DFormat = info.Video3DFormat
	source.VideoType = info.VideoType
	source.DefaultSubtitleStreamIndex = nil
	source.DefaultAudioStreamIndex = nil
	if audio := firstStream(source.MediaStreams, playback.StreamTypeAudio); audio != nil && audio.Index != -1 {
		index := audio.Index
		source.DefaultAudioStreamIndex = &index
	}
	if video := firstStream(source.MediaStreams, playback.StreamTypeVideo); video != nil {
		if video.BitRate == nil {
			if bitrate := estimatedVideoBitrate(video.Width); bitrate > 0 {
				video.BitRate = &bitrate
			}
		}
		// Upstream: "This is coming up false and preventing stream copy".
		video.IsAVC = nil
	}
	analyze := analyzeDurationMs
	source.AnalyzeDurationMs = &analyze
	total := 0
	for _, stream := range source.MediaStreams {
		if !stream.IsExternal && stream.BitRate != nil {
			total += *stream.BitRate
		}
	}
	if total > 0 {
		source.Bitrate = &total
	}
}

func firstStream(streams []playback.MediaStream, kind playback.MediaStreamType) *playback.MediaStream {
	for index := range streams {
		if streams[index].Type == kind {
			return &streams[index]
		}
	}
	return nil
}

// estimatedVideoBitrate is upstream's bitrate guess from the frame width;
// an unknown width counts as 1920.
func estimatedVideoBitrate(width *int) int {
	value := 1920
	if width != nil {
		value = *width
	}
	switch {
	case value >= 3000:
		return 30000000
	case value >= 1900:
		return 20000000
	case value >= 1200:
		return 8000000
	case value >= 700:
		return 2000000
	}
	return 0
}

// CloseLiveStream implements playback.LiveStreamProvider: one consumer
// fewer. An unknown id is not an error.
func (p *Provider) CloseLiveStream(_ context.Context, liveStreamID string) error {
	id := streamID(liveStreamID)
	p.options.Broker.Release(id)
	p.mu.Lock()
	if _, open := p.options.Broker.Info(id); !open {
		delete(p.opened, id)
	}
	p.mu.Unlock()
	return nil
}

// LiveStream implements playback.LiveTvSources.
func (p *Provider) LiveStream(_ context.Context, liveStreamID string) (playback.MediaSourceInfo, bool) {
	id := streamID(liveStreamID)
	p.mu.Lock()
	defer p.mu.Unlock()
	opened, found := p.opened[id]
	if !found {
		return playback.MediaSourceInfo{}, false
	}
	if _, open := p.options.Broker.Info(id); !open {
		delete(p.opened, id)
		return playback.MediaSourceInfo{}, false
	}
	return opened.source, true
}

// LiveStartup implements playback.LiveStartupSource: the startup times of
// the first open of a live stream.
func (p *Provider) LiveStartup(liveStreamID string) (playback.LiveStartup, bool) {
	id := streamID(liveStreamID)
	p.mu.Lock()
	opened, found := p.opened[id]
	p.mu.Unlock()
	if !found {
		return playback.LiveStartup{}, false
	}
	startup := opened.startup
	if info, open := p.options.Broker.Info(id); open {
		startup.Delivered = info.Delivered
	}
	return startup, true
}

// Reader opens a reader of the broker stream with unique id streamID, for
// GET /LiveTv/LiveStreamFiles/{streamId}/stream.{container}.
func (p *Provider) Reader(ctx context.Context, streamID string) (io.ReadCloser, error) {
	return p.options.Broker.Reader(ctx, streamID)
}

// sweep drops sources of streams the broker closed on its own (idle or
// upstream end). p.mu is held.
func (p *Provider) sweep() {
	for id := range p.opened {
		if _, open := p.options.Broker.Info(id); !open {
			delete(p.opened, id)
		}
	}
}

func (p *Provider) newKey() string {
	if p.options.NewKey != nil {
		return p.options.NewKey()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	return fmt.Sprintf("open-%d", p.next)
}

// tokenItemID reads the item id of an OpenToken:
// <md5 provider>_<type>_<item id>_<source id>.
func tokenItemID(token string) (dto.UUID, error) {
	parts := strings.Split(token, "_")
	if len(parts) != 4 || parts[0] != tuner.DotNetMD5(providerName) {
		return dto.NilUUID, fmt.Errorf("%w: unknown open token", playback.ErrItemNotFound)
	}
	id, err := dto.ParseUUID(parts[2])
	if err != nil {
		return dto.NilUUID, fmt.Errorf("%w: unknown open token", playback.ErrItemNotFound)
	}
	return id, nil
}

// streamID is the broker id at the end of a LiveStreamId.
func streamID(liveStreamID string) string {
	if index := strings.LastIndexByte(liveStreamID, '_'); index >= 0 {
		return liveStreamID[index+1:]
	}
	return liveStreamID
}
