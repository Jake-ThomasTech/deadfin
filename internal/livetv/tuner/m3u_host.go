// SPDX-License-Identifier: GPL-2.0-only

package tuner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/secret"
	"golang.org/x/sync/singleflight"
)

// MinRefreshInterval is the shortest time between two playlist downloads of
// one tuner, whatever triggers them (docs/spikes/livetv.md §4.3). A call
// inside the interval returns the channels of the last download.
const MinRefreshInterval = 15 * time.Minute

// M3UHost is the "m3u" tuner type (M3UTunerHost.cs). Its Url is a local
// playlist file or an HTTP(S) URL; a local HDHomeRun works through its
// /lineup.m3u.
type M3UHost struct {
	fetch *fetch.Client
	clock clock.Clock
	hls   HLSRemuxer

	group singleflight.Group
	mu    sync.Mutex
	cache map[string]m3uDownload
	// refreshed holds the playlist that failed streams last re-read, per
	// tuner; renewed holds the new URL of each channel whose URL expired.
	refreshed map[string]m3uDownload
	renewed   map[string]string
}

type m3uDownload struct {
	at       time.Time
	channels []Channel
}

// HLSRemuxer reads a live HLS source as one MPEG-TS byte stream; the
// caller closes it. cmd/deadfin adapts ffmpeg.LiveRemuxer.
type HLSRemuxer interface {
	RemuxHLS(ctx context.Context, url, userAgent string) (io.ReadCloser, error)
}

// NewM3UHost returns the M3U tuner type. A nil hls leaves HLS channels
// unsupported.
func NewM3UHost(client *fetch.Client, clk clock.Clock, hls HLSRemuxer) *M3UHost {
	return &M3UHost{fetch: client, clock: clk, hls: hls, cache: make(map[string]m3uDownload), refreshed: make(map[string]m3uDownload), renewed: make(map[string]string)}
}

// Type implements Host.
func (*M3UHost) Type() string { return "m3u" }

// Name implements Host (M3UTunerHost.cs:62).
func (*M3UHost) Name() string { return "M3U Tuner" }

// SourceKey names the politeness and cache state of a tuner. An edit of the
// URL or User-Agent starts a new state.
func SourceKey(tuner config.TunerHostInfo) string {
	return tuner.Id + "\x00" + tuner.Url + "\x00" + tuner.UserAgent
}

// Channels implements Host. Inside MinRefreshInterval of the last download
// it returns the cached list. When a download fails and an earlier list
// exists, it returns that list with the error. Every error has the tuner
// credentials masked. The cache holds the whole playlist; the tuner's group
// filter applies to each list returned (D-91), so a filter edit needs no
// download.
func (h *M3UHost) Channels(ctx context.Context, tuner config.TunerHostInfo) ([]Channel, error) {
	channels, err := h.allChannels(ctx, tuner)
	if channels == nil {
		return nil, err
	}
	return FilterGroups(tuner, channels), err
}

// Groups counts the channels per group-title in the tuner's cached
// playlist, before the group filter. It never downloads; ok is false when
// no playlist is cached yet.
func (h *M3UHost) Groups(tuner config.TunerHostInfo) (groups []GroupCount, ok bool) {
	h.mu.Lock()
	last, cached := h.cache[SourceKey(tuner)]
	h.mu.Unlock()
	if !cached {
		return []GroupCount{}, false
	}
	return CountGroups(last.channels), true
}

func (h *M3UHost) allChannels(ctx context.Context, tuner config.TunerHostInfo) ([]Channel, error) {
	key := SourceKey(tuner)
	now := h.clock.Now()
	h.mu.Lock()
	last, cached := h.cache[key]
	h.mu.Unlock()
	if cached && now.Sub(last.at) < RefreshInterval(tuner) {
		return last.channels, nil
	}

	// Concurrent callers for one tuner share one download.
	result, err, _ := h.group.Do(key, func() (any, error) {
		return h.download(ctx, tuner, key)
	})
	channels, _ := result.([]Channel)
	if err != nil {
		if cached {
			return last.channels, err
		}
		return nil, err
	}
	h.mu.Lock()
	h.cache[key] = m3uDownload{at: now, channels: channels}
	h.mu.Unlock()
	return channels, nil
}

// DefaultStreamOpenInterval is the shortest time between two stream
// connects to one tuner's source when StreamOpenIntervalMs is unset (D-93).
const DefaultStreamOpenInterval = time.Second

// StreamOpenInterval is the shortest time between two stream connects to a
// tuner's source: StreamOpenIntervalMs, DefaultStreamOpenInterval when it
// is unset, and no wait when it is 0 or less (D-93).
func StreamOpenInterval(tuner config.TunerHostInfo) time.Duration {
	if tuner.StreamOpenIntervalMs == nil {
		return DefaultStreamOpenInterval
	}
	return max(time.Duration(*tuner.StreamOpenIntervalMs)*time.Millisecond, 0)
}

// RefreshInterval is the shortest time between two playlist downloads of a
// tuner: PlaylistRefreshMinutes, never under MinRefreshInterval (D-78).
func RefreshInterval(tuner config.TunerHostInfo) time.Duration {
	if tuner.PlaylistRefreshMinutes == nil {
		return MinRefreshInterval
	}
	return max(time.Duration(*tuner.PlaylistRefreshMinutes)*time.Minute, MinRefreshInterval)
}

// Forget drops the cached channels and the fetch state of a tuner, after
// the tuner is saved or deleted. A save is how an administrator retries a
// source that is rejected or backing off.
func (h *M3UHost) Forget(tuner config.TunerHostInfo) {
	key := SourceKey(tuner)
	h.mu.Lock()
	delete(h.cache, key)
	delete(h.refreshed, key)
	for id := range h.renewed {
		if strings.HasPrefix(id, key+"\x00") {
			delete(h.renewed, id)
		}
	}
	h.mu.Unlock()
	h.fetch.Reset(key)
}

func (h *M3UHost) download(ctx context.Context, tuner config.TunerHostInfo, key string) ([]Channel, error) {
	if strings.TrimSpace(tuner.Url) == "" {
		return nil, ErrNoTunerURL
	}
	secrets := secret.Collect([]string{tuner.Url})
	body, err := h.open(ctx, tuner, key, secrets)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	var channels []Channel
	options := ParseOptions{SkipVOD: tuner.SkipVodEntries != nil && *tuner.SkipVodEntries}
	err = ParseM3U(body, M3UChannelIDPrefixFor(tuner.Url), tuner.Id, options, func(channel Channel) error {
		channels = append(channels, channel)
		return nil
	})
	if err != nil {
		return nil, secrets.Err(fmt.Errorf("m3u tuner: %w", err))
	}
	if channels == nil {
		channels = []Channel{}
	}
	return channels, nil
}

// open ports M3uParser.GetListingsStream (M3uParser.cs:48-70): a Url that
// does not start with "http" is a local file.
func (h *M3UHost) open(ctx context.Context, tuner config.TunerHostInfo, key string, secrets secret.Set) (io.ReadCloser, error) {
	if !hasPrefixFold(tuner.Url, "http") {
		file, err := os.Open(tuner.Url)
		if err != nil {
			return nil, secrets.Err(fmt.Errorf("m3u tuner: %w", err))
		}
		return file, nil
	}
	response, err := h.fetch.Get(ctx, fetch.Request{
		Key:       key,
		URL:       tuner.Url,
		UserAgent: strings.TrimSpace(tuner.UserAgent),
		Secrets:   secrets,
	})
	if err != nil {
		return nil, fmt.Errorf("m3u tuner: %w", err)
	}
	return response.Body, nil
}

// ErrNoTunerURL reports a tuner without a Url.
var ErrNoTunerURL = errors.New("m3u tuner: url is required")

// Validate ports M3UTunerHost.Validate (M3UTunerHost.cs:132-137): the
// playlist must open. It reads the channels through Channels, so a refresh
// soon after the save reuses that download.
func (h *M3UHost) Validate(ctx context.Context, tuner config.TunerHostInfo) error {
	_, err := h.Channels(ctx, tuner)
	return err
}

// ErrUnsupportedStream reports a channel URL the broker cannot read yet:
// rtsp, rtp and udp channels, and HLS channels until the HLS ingest exists.
var ErrUnsupportedStream = errors.New("m3u tuner: unsupported channel stream")

// Owns implements Streamer: M3U channel ids start with
// "m3u_" + md5(tuner URL) (M3UTunerHost.cs:64-67).
func (*M3UHost) Owns(tuner config.TunerHostInfo, channelID string) bool {
	return strings.HasPrefix(channelID, M3UChannelIDPrefixFor(tuner.Url))
}

// StreamRefreshInterval is the shortest time between two playlist re-reads
// that failed streams of one tuner trigger (docs/spikes/livetv.md §0.1).
const StreamRefreshInterval = 5 * time.Minute

// Stream implements Streamer for HTTP(S) channels. It sends the tuner's
// User-Agent override or the application User-Agent (D-71). A 401 on a
// stream rejects the tuner, so its playlist and streams stop together. A
// 403, 404 or 410 means the channel URL expired: the tuner's playlist is
// re-read once and the stream retried at the channel's new URL (D-87).
func (h *M3UHost) Stream(ctx context.Context, tuner config.TunerHostInfo, channel Channel) (io.ReadCloser, error) {
	renewedKey := SourceKey(tuner) + "\x00" + channel.ID
	h.mu.Lock()
	if path, found := h.renewed[renewedKey]; found {
		channel.Path = path
	}
	h.mu.Unlock()
	body, err := h.stream(ctx, tuner, channel)
	if !expiredStream(err) {
		return body, err
	}
	path, ok := h.refreshChannel(ctx, tuner, channel)
	if !ok {
		return nil, err
	}
	h.mu.Lock()
	h.renewed[renewedKey] = path
	h.mu.Unlock()
	channel.Path = path
	return h.stream(ctx, tuner, channel)
}

func (h *M3UHost) stream(ctx context.Context, tuner config.TunerHostInfo, channel Channel) (io.ReadCloser, error) {
	if !hasPrefixFold(channel.Path, "http://") && !hasPrefixFold(channel.Path, "https://") {
		return nil, ErrUnsupportedStream
	}
	key := SourceKey(tuner)
	if h.fetch.State(key) == fetch.StateCredentialsRejected {
		return nil, fmt.Errorf("m3u tuner: stream: fetch: %w", fetch.ErrCredentialsRejected)
	}
	secrets := secret.Collect([]string{tuner.Url, channel.Path})
	// Each stream URL keeps its own politeness state: an expired token on
	// one channel must not hold back the playlist re-read that renews it.
	response, err := h.fetch.Get(ctx, fetch.Request{
		Key:       key + "\x00stream\x00" + channel.Path,
		URL:       channel.Path,
		UserAgent: strings.TrimSpace(tuner.UserAgent),
		Secrets:   secrets,
	})
	if errors.Is(err, fetch.ErrCredentialsRejected) {
		h.fetch.Reject(key)
	}
	if err != nil {
		return nil, fmt.Errorf("m3u tuner: stream: %w", err)
	}
	if !isHLSResponse(channel.Path, response.Header.Get("Content-Type")) {
		return response.Body, nil
	}
	// An HLS channel: the playlist request above passed the source's
	// politeness and credential checks. One ffmpeg reads the playlist from
	// its final URL and remuxes it for the broker, so every viewer shares
	// one ingest (docs/spikes/livetv.md §0.1).
	_ = response.Body.Close()
	if h.hls == nil {
		return nil, ErrUnsupportedStream
	}
	playlist := channel.Path
	if response.Request != nil && response.Request.URL != nil {
		playlist = response.Request.URL.String()
	}
	userAgent := strings.TrimSpace(tuner.UserAgent)
	if userAgent == "" {
		userAgent = h.fetch.UserAgent()
	}
	body, err := h.hls.RemuxHLS(ctx, playlist, userAgent)
	if err != nil {
		return nil, secret.Collect([]string{tuner.Url, channel.Path, playlist}).Err(fmt.Errorf("m3u tuner: hls stream: %w", err))
	}
	return body, nil
}

// expiredStream reports a stream refused with 403, 404 or 410.
func expiredStream(err error) bool {
	var status *fetch.StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.StatusCode {
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

// refreshChannel re-reads the tuner's playlist for a channel whose URL
// failed and returns the channel's new URL. The channel is found by its
// tuner channel id (tvg-id or channel-id), or else by name and number,
// because its id follows its URL. Concurrent failures share one download,
// and a tuner's playlist is re-read at most once per
// StreamRefreshInterval; inside the interval the last re-read list
// answers. The re-read list stays apart from the playlist cache, so stored
// channel ids keep resolving until the guide refresh.
func (h *M3UHost) refreshChannel(ctx context.Context, tuner config.TunerHostInfo, failed Channel) (string, bool) {
	key := SourceKey(tuner)
	result, _, _ := h.group.Do("refresh\x00"+key, func() (any, error) {
		now := h.clock.Now()
		h.mu.Lock()
		last, recent := h.refreshed[key]
		h.mu.Unlock()
		if recent && now.Sub(last.at) < StreamRefreshInterval {
			return last.channels, nil
		}
		channels, err := h.download(ctx, tuner, key)
		if err != nil {
			channels = last.channels
		}
		h.mu.Lock()
		h.refreshed[key] = m3uDownload{at: now, channels: channels}
		h.mu.Unlock()
		return channels, nil
	})
	channels, _ := result.([]Channel)
	for _, channel := range channels {
		if sameChannel(channel, failed) && channel.Path != failed.Path {
			return channel.Path, true
		}
	}
	return "", false
}

// sameChannel reports one playlist entry across URL changes.
func sameChannel(a, b Channel) bool {
	if a.TunerChannelID != "" || b.TunerChannelID != "" {
		return a.TunerChannelID == b.TunerChannelID
	}
	return a.Name == b.Name && a.Number == b.Number
}

// isHLSResponse reports an HLS playlist by its path extension or media type.
func isHLSResponse(rawURL, contentType string) bool {
	path := rawURL
	if parsed, err := url.Parse(rawURL); err == nil {
		path = parsed.Path
	}
	if strings.HasSuffix(strings.ToLower(path), ".m3u8") {
		return true
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return mediaType == "application/vnd.apple.mpegurl" || mediaType == "application/x-mpegurl" || mediaType == "audio/mpegurl" || mediaType == "audio/x-mpegurl"
}
