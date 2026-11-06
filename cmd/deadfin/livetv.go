// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/itemstore"
	"github.com/deadfin-org/deadfin/internal/livetv/media"
	"github.com/deadfin-org/deadfin/internal/livetv/stream"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/probe"
	"github.com/deadfin-org/deadfin/internal/session"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

// liveTvUserAgent is the application User-Agent of every provider request
// (docs/spikes/livetv.md §4.3).
const liveTvUserAgent = "Deadfin/dev (+https://github.com/deadfin-org/deadfin)"

// newLiveTv composes the Live TV guide service: the M3U tuner type, the
// XMLTV listing provider, both behind one polite fetch client. Live TV stays
// off until an administrator adds a tuner. A configuration change queues
// Refresh Guide through queue.
func newLiveTv(manager *config.Manager, items *store.ItemStore, paths config.Paths, clk clock.Clock, queue func()) (*livetv.Service, error) {
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: clk, UserAgent: liveTvUserAgent})
	if err != nil {
		return nil, fmt.Errorf("live tv fetch client: %w", err)
	}
	return livetv.New(livetv.Options{
		Config:   manager,
		Tuners:   tuner.NewRegistry(tuner.NewM3UHost(client, clk, liveHLSRemuxer{remuxer: ffmpeg.LiveRemuxer{Path: "ffmpeg"}})),
		Listings: []livetv.ListingsProvider{guide.NewXMLTV(client, clk, paths.Cache)},
		Items:    itemstore.New(items),
		Clock:    clk,
		Logger:   slog.Default(),
		Paths:    livetv.Paths{ProgramData: paths.Root, InternalMetadata: paths.Metadata, Cache: paths.Cache},
		MetadataLanguage: func() string {
			return manager.System().PreferredMetadataLanguage
		},
		Queue: queue,
	})
}

// liveHLSRemuxer adapts ffmpeg.LiveRemuxer to tuner.HLSRemuxer.
type liveHLSRemuxer struct{ remuxer ffmpeg.LiveRemuxer }

func (r liveHLSRemuxer) RemuxHLS(ctx context.Context, url, userAgent string) (io.ReadCloser, error) {
	return r.remuxer.Remux(ctx, encoding.LiveInput{URL: url, UserAgent: userAgent})
}

// queueGuideRefresh ports ITaskManager.CancelIfRunningAndQueue for Refresh
// Guide: a running refresh is cancelled and a new run is queued behind it.
func queueGuideRefresh(scheduler *tasks.Scheduler) {
	if err := scheduler.Cancel(tasks.RefreshGuideID); err != nil {
		slog.Default().Warn("cancel guide refresh", "error", err)
	}
	if err := scheduler.Run(tasks.RefreshGuideID); err != nil {
		slog.Default().Warn("queue guide refresh", "error", err)
	}
}

// guideTask adapts livetv.Service to tasks.GuideRefresher.
type guideTask struct{ service *livetv.Service }

func (t guideTask) RefreshGuide(ctx context.Context, progress func(float64)) error {
	_, err := t.service.RefreshGuide(ctx, progress)
	return err
}

func (t guideTask) Hidden() bool { return t.service.Hidden() }

// newLiveStreams composes the live stream broker and the playback adapter
// of the Live TV channels. Buffer files live under the transcode path, as
// upstream's do.
func newLiveStreams(service *livetv.Service, paths config.Paths, clk clock.Clock, localURL string, network playbackNetworkClassifier, sessions *session.Manager) (*stream.Broker, *media.Provider, error) {
	broker, err := stream.New(stream.Options{Dir: paths.Transcodes, Clock: clk, Logger: slog.Default()})
	if err != nil {
		return nil, nil, fmt.Errorf("live stream broker: %w", err)
	}
	provider, err := media.New(media.Options{
		Channels: service,
		Broker:   broker,
		LocalURL: func() string { return localURL },
		IsLocalHost: func(host string) bool {
			addr, err := netip.ParseAddr(host)
			return err == nil && network.IsLAN(addr)
		},
		Probe:         probeLiveStream,
		Clock:         clk,
		Logger:        slog.Default(),
		ReleaseDevice: sessions.ReleaseDeviceLiveStreams,
		ReopenDevice:  sessions.ReopenDeviceLiveStream,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("live tv media sources: %w", err)
	}
	return broker, provider, nil
}

// probeLiveStream probes an opened broker stream as upstream
// MediaEncoder.GetMediaInfo does for a live source: ffprobe reads the
// broker URL with a 3000 ms analyze duration.
func probeLiveStream(ctx context.Context, url string) (playback.MediaSourceInfo, error) {
	prober, err := ffmpeg.NewProber(ctx, applicationFFProbePath)
	if err != nil {
		return playback.MediaSourceInfo{}, fmt.Errorf("open ffprobe: %w", err)
	}
	prober.AnalyzeDuration = 3_000_000
	raw, err := prober.ProbeAndValidate(ctx, url)
	if err != nil {
		return playback.MediaSourceInfo{}, err
	}
	info, err := probe.Normalize(ctx, raw, probe.Options{Path: url, Protocol: "Http", VideoType: "VideoFile"})
	if err != nil {
		return playback.MediaSourceInfo{}, err
	}
	return info.MediaSourceInfo, nil
}

// liveStreamLocalURL is the base URL ffmpeg uses to read a broker stream:
// the listen address when it names one IP, else loopback, with the
// configured BaseUrl.
func liveStreamLocalURL(listen, base string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1" + base
	}
	if addr, err := netip.ParseAddr(host); err != nil || addr.IsUnspecified() {
		host = "127.0.0.1"
	}
	if value, err := strconv.Atoi(port); err != nil || value <= 0 {
		return "http://" + net.JoinHostPort(host, "8096") + base
	}
	return "http://" + net.JoinHostPort(host, port) + base
}
