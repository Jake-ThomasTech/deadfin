// SPDX-License-Identifier: GPL-2.0-only

package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/stream"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/deadfin-org/deadfin/internal/playback"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// secretURL is a channel URL with marker credentials; no response may
// carry any part of it.
const secretURL = "http://panel.invalid/live/MARKERUSER/MARKERPASS/1.ts"

type fakeChannels struct {
	mu      sync.Mutex
	tuner   config.TunerHostInfo
	itemID  dto.UUID
	opens   int
	streams []*io.PipeWriter
	// clock, when set, stamps each OpenStream call into connects.
	clock    clock.Clock
	connects []time.Time
}

func (f *fakeChannels) ChannelStream(_ context.Context, itemID dto.UUID) (livetv.ChannelStream, bool, error) {
	if itemID != f.itemID {
		return livetv.ChannelStream{}, false, nil
	}
	external := "m3u_abc"
	return livetv.ChannelStream{
		Item:    item.Item{ID: itemID, Kind: item.KindLiveTvChannel, ExternalID: &external},
		Tuner:   f.tuner,
		Channel: tuner.Channel{ID: external, Path: secretURL, TunerHostID: f.tuner.Id},
	}, true, nil
}

func (f *fakeChannels) OpenStream(context.Context, livetv.ChannelStream) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	f.mu.Lock()
	f.opens++
	f.streams = append(f.streams, writer)
	if f.clock != nil {
		f.connects = append(f.connects, f.clock.Now())
	}
	f.mu.Unlock()
	go func() { _, _ = writer.Write([]byte("ts")) }()
	return reader, nil
}

func newProvider(t *testing.T, info config.TunerHostInfo) (*Provider, *fakeChannels) {
	t.Helper()
	broker, err := stream.New(stream.Options{Dir: t.TempDir(), Clock: clock.NewFake(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	channels := &fakeChannels{tuner: info, itemID: dto.UUID{1}}
	provider, err := New(Options{Channels: channels, Broker: broker, LocalURL: func() string { return "http://127.0.0.1:8096" }})
	if err != nil {
		t.Fatal(err)
	}
	return provider, channels
}

func assertNoSecret(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"MARKERUSER", "MARKERPASS", "panel.invalid"} {
		if strings.Contains(string(raw), marker) {
			t.Fatalf("response carries %q: %s", marker, raw)
		}
	}
}

func TestChannelSources(t *testing.T) {
	provider, channels := newProvider(t, config.TunerHostInfo{Id: "t1", TunerCount: 2, IgnoreDts: true})
	sources, ok, err := provider.ChannelSources(context.Background(), channels.itemID)
	if err != nil || !ok || len(sources) != 1 {
		t.Fatalf("sources %v %v %v", sources, ok, err)
	}
	source := sources[0]
	if source.Path != "" || !source.RequiresOpening || !source.RequiresClosing || !source.IsInfiniteStream ||
		source.SupportsDirectPlay || !source.SupportsDirectStream || !source.IgnoreDts || source.ID != tuner.DotNetMD5(secretURL) {
		t.Fatalf("source %+v", source)
	}
	want := tuner.DotNetMD5(providerName) + "_LiveTvChannel_" + channels.itemID.String() + "_" + source.ID
	if source.OpenToken != want {
		t.Fatalf("open token %q, want %q", source.OpenToken, want)
	}
	if len(source.MediaStreams) != 2 || source.MediaStreams[0].Index != -1 || !source.MediaStreams[0].IsInterlaced {
		t.Fatalf("streams %+v", source.MediaStreams)
	}
	assertNoSecret(t, sources)

	if _, ok, err := provider.ChannelSources(context.Background(), dto.UUID{2}); ok || err != nil {
		t.Fatalf("not a channel: %v %v", ok, err)
	}
}

func TestOpenShareAndClose(t *testing.T) {
	provider, channels := newProvider(t, config.TunerHostInfo{Id: "t1", TunerCount: 1, AllowStreamSharing: true})
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	request := playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken}

	first, err := provider.OpenLiveStream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	source := first.MediaSource
	if !strings.HasPrefix(source.Path, "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/") || !strings.HasSuffix(source.Path, "/stream.ts") ||
		source.LiveStreamID == "" || source.AnalyzeDurationMs == nil || *source.AnalyzeDurationMs != 3000 {
		t.Fatalf("opened %+v", source)
	}
	assertNoSecret(t, first)

	second, err := provider.OpenLiveStream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.MediaSource.LiveStreamID != source.LiveStreamID || channels.opens != 1 {
		t.Fatalf("not shared: %s %s opens %d", second.MediaSource.LiveStreamID, source.LiveStreamID, channels.opens)
	}
	if got, ok := provider.LiveStream(context.Background(), source.LiveStreamID); !ok || got.Path != source.Path {
		t.Fatalf("lookup %v %+v", ok, got)
	}

	reader, err := provider.Reader(context.Background(), streamID(source.LiveStreamID))
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2)
	if _, err := io.ReadFull(reader, buffer); err != nil || string(buffer) != "ts" {
		t.Fatalf("read %q %v", buffer, err)
	}
	_ = reader.Close()

	_ = provider.CloseLiveStream(context.Background(), source.LiveStreamID)
	if _, ok := provider.LiveStream(context.Background(), source.LiveStreamID); !ok {
		t.Fatal("closed with a consumer left")
	}
	_ = provider.CloseLiveStream(context.Background(), source.LiveStreamID)
	if _, ok := provider.LiveStream(context.Background(), source.LiveStreamID); ok {
		t.Fatal("still open")
	}
	if err := provider.CloseLiveStream(context.Background(), "unknown"); err != nil {
		t.Fatal(err)
	}
}

// Without stream sharing, each open uses its own tuner; the second open on
// a one-tuner host is busy.
func TestOpenBusyWithoutSharing(t *testing.T) {
	provider, channels := newProvider(t, config.TunerHostInfo{Id: "t1", TunerCount: 1})
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	request := playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken}
	if _, err := provider.OpenLiveStream(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.OpenLiveStream(context.Background(), request); !errors.Is(err, playback.ErrLiveStreamBusy) {
		t.Fatalf("second open: %v", err)
	}
}

func TestOpenRejectsUnknownToken(t *testing.T) {
	provider, _ := newProvider(t, config.TunerHostInfo{Id: "t1"})
	for _, token := range []string{"", "x_y_z", tuner.DotNetMD5(providerName) + "_LiveTvChannel_notauuid_1"} {
		if _, err := provider.OpenLiveStream(context.Background(), playback.OpenLiveStreamRequest{OpenToken: token}); !errors.Is(err, playback.ErrItemNotFound) {
			t.Fatalf("token %q: %v", token, err)
		}
	}
}

// The first open waits on the clock, probes the broker URL and keeps the
// probed stream indexes; a viewer that joins the shared stream gets the
// same source without a second probe.
func TestOpenProbes(t *testing.T) {
	broker, err := stream.New(stream.Options{Dir: t.TempDir(), Clock: clock.NewFake(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	channels := &fakeChannels{tuner: config.TunerHostInfo{Id: "t1", TunerCount: 1, AllowStreamSharing: true}, itemID: dto.UUID{1}}
	fake := clock.NewFake(time.Unix(0, 0))
	var probed []string
	width := 1280
	provider, err := New(Options{
		Channels: channels, Broker: broker, Clock: fake,
		LocalURL: func() string { return "http://127.0.0.1:8096" },
		Probe: func(_ context.Context, url string) (playback.MediaSourceInfo, error) {
			probed = append(probed, url)
			return playback.MediaSourceInfo{Container: "mpegts", MediaStreams: []playback.MediaStream{
				{Type: playback.StreamTypeVideo, Index: 0, Codec: "h264", Width: &width},
				{Type: playback.StreamTypeAudio, Index: 1, Codec: "aac", BitRate: intPtr(128000)},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	request := playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken}

	source := openAdvancing(t, provider, fake, request)
	if len(probed) != 1 || probed[0] != source.Path {
		t.Fatalf("probed %v, path %s", probed, source.Path)
	}
	if source.Container != "mpegts" || len(source.MediaStreams) != 2 || source.MediaStreams[0].Index != 0 ||
		source.DefaultAudioStreamIndex == nil || *source.DefaultAudioStreamIndex != 1 || source.RunTimeTicks != nil {
		t.Fatalf("source %+v", source)
	}
	if video := source.MediaStreams[0]; video.BitRate == nil || *video.BitRate != 8000000 {
		t.Fatalf("video bitrate %v", video.BitRate)
	}
	if source.Bitrate == nil || *source.Bitrate != 8128000 {
		t.Fatalf("bitrate %v", source.Bitrate)
	}

	joined, err := provider.OpenLiveStream(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(probed) != 1 || joined.MediaSource.Container != "mpegts" {
		t.Fatalf("join probed again: %v %+v", probed, joined.MediaSource)
	}
	if got, ok := provider.LiveStream(context.Background(), source.LiveStreamID); !ok || len(got.MediaStreams) != 2 {
		t.Fatalf("lookup %v %+v", ok, got)
	}
	// The startup times are the first open's: the probe waits probeDelay
	// after the request, and the broker times the first data.
	startup, ok := provider.LiveStartup(source.LiveStreamID)
	if !ok || startup.Probed.Sub(startup.Received) < probeDelay || startup.Connected.IsZero() || startup.FirstByte.IsZero() ||
		startup.Delivered == 0 {
		t.Fatalf("startup %v %+v", ok, startup)
	}
	if _, ok := provider.LiveStartup("unknown"); ok {
		t.Fatal("startup of an unknown stream")
	}
}

// A failed probe keeps the unprobed source, as upstream does.
func TestOpenProbeFailureKeepsSource(t *testing.T) {
	broker, err := stream.New(stream.Options{Dir: t.TempDir(), Clock: clock.NewFake(time.Unix(0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	channels := &fakeChannels{tuner: config.TunerHostInfo{Id: "t1"}, itemID: dto.UUID{1}}
	fake := clock.NewFake(time.Unix(0, 0))
	provider, err := New(Options{
		Channels: channels, Broker: broker, Clock: fake,
		LocalURL: func() string { return "http://127.0.0.1:8096" },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Probe: func(context.Context, string) (playback.MediaSourceInfo, error) {
			return playback.MediaSourceInfo{}, errors.New("no data")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	source := openAdvancing(t, provider, fake, playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken})
	if source.LiveStreamID == "" || source.MediaStreams[0].Index != -1 {
		t.Fatalf("source %+v", source)
	}
}

// A cancelled open during the probe wait releases the stream.
func TestOpenProbeCancelled(t *testing.T) {
	provider, channels := newProvider(t, config.TunerHostInfo{Id: "t1", TunerCount: 1})
	provider.options.Clock = clock.NewFake(time.Unix(0, 0))
	provider.options.Probe = func(context.Context, string) (playback.MediaSourceInfo, error) {
		t.Error("probed after cancel")
		return playback.MediaSourceInfo{}, nil
	}
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := provider.OpenLiveStream(ctx, playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken})
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("open: %v", err)
	}
	if streams := provider.options.Broker.InUse("t1"); streams != 0 {
		t.Fatalf("tuner still in use: %d", streams)
	}
}

// openAdvancing opens a stream while it moves the fake clock past the
// probe wait.
func openAdvancing(t *testing.T, provider *Provider, fake *clock.Fake, request playback.OpenLiveStreamRequest) playback.MediaSourceInfo {
	t.Helper()
	type result struct {
		response playback.LiveStreamResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := provider.OpenLiveStream(context.Background(), request)
		done <- result{response, err}
	}()
	for {
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.response.MediaSource
		default:
			fake.Advance(time.Second)
			runtime.Gosched()
		}
	}
}

func intPtr(value int) *int { return &value }

// D-92: an open by a device first releases the device's streams on the same
// tuner, except a shared stream of the same channel, which it joins.
func TestOpenReleasesTheDevicesOtherStreams(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sharing bool
		device  string
		want    []bool // filter answers for the held stream
		busy    bool
	}{
		{name: "unshared reopen frees the tuner", device: "tv", want: []bool{true}},
		{name: "shared same channel is joined", sharing: true, device: "tv", want: []bool{false}},
		{name: "no device releases nothing", busy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The reopen connects at once: no D-93 interval on the fake clock.
			noInterval := 0
			provider, channels := newProvider(t, config.TunerHostInfo{Id: "t1", TunerCount: 1, AllowStreamSharing: tc.sharing, StreamOpenIntervalMs: &noInterval})
			sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
			request := playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken, DeviceID: tc.device}
			first, err := provider.OpenLiveStream(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			var answers []bool
			provider.options.ReleaseDevice = func(ctx context.Context, deviceID string, release func(string) bool) {
				if deviceID != tc.device {
					t.Errorf("device %q", deviceID)
				}
				if release("other_tuner_unknown") {
					t.Error("released a stream that is not open")
				}
				answer := release(first.MediaSource.LiveStreamID)
				answers = append(answers, answer)
				if answer {
					_ = provider.CloseLiveStream(ctx, first.MediaSource.LiveStreamID)
				}
			}
			_, err = provider.OpenLiveStream(context.Background(), request)
			if tc.busy != errors.Is(err, playback.ErrLiveStreamBusy) || (!tc.busy && err != nil) {
				t.Fatalf("second open: %v", err)
			}
			if !slices.Equal(answers, tc.want) {
				t.Fatalf("filter answers %v, want %v", answers, tc.want)
			}
		})
	}
}

// D-93: the tuner's StreamOpenIntervalMs spaces the provider connects of
// two opens on the broker clock.
func TestOpenWaitsForTheStreamOpenInterval(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	// The idle, open and stall timeouts are far beyond the fake time the
	// test advances. With the defaults, a second open that the scheduler
	// starts late let a stall reconnect of the first stream take the
	// second connect, and the test hung (#802).
	broker, err := stream.New(stream.Options{Dir: t.TempDir(), Clock: clk, IdleTimeout: 24 * time.Hour, OpenTimeout: 24 * time.Hour, StallTimeout: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close(context.Background()) })
	interval := 500
	channels := &fakeChannels{tuner: config.TunerHostInfo{Id: "t1", TunerCount: 2, StreamOpenIntervalMs: &interval}, itemID: dto.UUID{1}, clock: clk}
	provider, err := New(Options{Channels: channels, Broker: broker, LocalURL: func() string { return "http://127.0.0.1:8096" }})
	if err != nil {
		t.Fatal(err)
	}
	sources, _, _ := provider.ChannelSources(context.Background(), channels.itemID)
	request := playback.OpenLiveStreamRequest{OpenToken: sources[0].OpenToken}
	if _, err := provider.OpenLiveStream(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := provider.OpenLiveStream(context.Background(), request)
		done <- err
	}()
	// The second open arms its connect timer only when the scheduler runs
	// it, so the clock advances until the open returns. The limit stays
	// below the broker timeouts and fails a missed connect fast.
	const limit = time.Hour
	for start := clk.Now(); ; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			channels.mu.Lock()
			connects := slices.Clone(channels.connects)
			channels.mu.Unlock()
			if len(connects) != 2 {
				t.Fatalf("connects = %d, want 2", len(connects))
			}
			if gap := connects[1].Sub(connects[0]); gap < 500*time.Millisecond {
				t.Fatalf("second open connected after %v, want 500ms", gap)
			}
			return
		default:
			if clk.Now().Sub(start) >= limit {
				t.Fatalf("second open did not return within %v of fake time", limit)
			}
			clk.Advance(10 * time.Millisecond)
			runtime.Gosched()
		}
	}
}
