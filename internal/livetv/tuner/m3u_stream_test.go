// SPDX-License-Identifier: GPL-2.0-only

package tuner_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// streamPanel serves channel streams: /live/<user>/<pass>/1.ts as TS,
// /hls/1.m3u8 as a playlist, and the status in status for every path.
type streamPanel struct {
	server *httptest.Server
	mu     sync.Mutex
	status int
	agents []string
}

func newStreamPanel(t *testing.T) *streamPanel {
	t.Helper()
	p := &streamPanel{status: http.StatusOK}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		status := p.status
		p.agents = append(p.agents, r.UserAgent())
		p.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n")
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = io.WriteString(w, "tsbytes")
	}))
	t.Cleanup(p.server.Close)
	return p
}

func TestM3UHostStream(t *testing.T) {
	p := newPanel(t)
	streams := newStreamPanel(t)
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	info.UserAgent = "IPTVSmarters/1.0"
	channel := tuner.Channel{ID: tuner.M3UChannelIDPrefixFor(info.Url) + "x", Path: streams.server.URL + "/live/alice/" + marker + "/1.ts"}

	if !host.Owns(info, channel.ID) || host.Owns(info, "m3u_other") {
		t.Fatal("Owns does not follow the tuner prefix")
	}
	body, err := host.Stream(t.Context(), info, channel)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "tsbytes" || streams.agents[0] != "IPTVSmarters/1.0" {
		t.Fatalf("stream %q, agents %v", got, streams.agents)
	}

	hls := channel
	hls.Path = streams.server.URL + "/hls/1.m3u8"
	if _, err := host.Stream(t.Context(), info, hls); !errors.Is(err, tuner.ErrUnsupportedStream) {
		t.Fatalf("hls: %v", err)
	}
	udp := channel
	udp.Path = "udp://239.0.0.1:1234"
	if _, err := host.Stream(t.Context(), info, udp); !errors.Is(err, tuner.ErrUnsupportedStream) {
		t.Fatalf("udp: %v", err)
	}
}

// A 401 on a stream marks the tuner's source rejected: no further playlist
// or stream request goes out, and no error carries the credentials.
func TestM3UHostStreamRejected(t *testing.T) {
	p := newPanel(t)
	streams := newStreamPanel(t)
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	channel := tuner.Channel{ID: tuner.M3UChannelIDPrefixFor(info.Url) + "x", Path: streams.server.URL + "/live/alice/" + marker + "/1.ts"}

	streams.mu.Lock()
	streams.status = http.StatusUnauthorized
	streams.mu.Unlock()
	_, err := host.Stream(t.Context(), info, channel)
	if !errors.Is(err, fetch.ErrCredentialsRejected) || strings.Contains(err.Error(), marker) {
		t.Fatalf("stream 401: %v", err)
	}
	if _, err := host.Stream(t.Context(), info, channel); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("second stream: %v", err)
	}
	if _, err := host.Channels(t.Context(), info); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("playlist after a stream 401: %v", err)
	}
	if len(streams.agents) != 1 || p.requests.Load() != 0 {
		t.Fatalf("requests after the 401: streams %d, playlist %d", len(streams.agents), p.requests.Load())
	}
}

// fakeRemuxer records HLS remux calls.
type fakeRemuxer struct {
	mu     sync.Mutex
	calls  []string
	agents []string
	err    error
}

func (f *fakeRemuxer) RemuxHLS(_ context.Context, url, userAgent string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	f.agents = append(f.agents, userAgent)
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader("remuxed")), nil
}

func newHLSHost(t *testing.T, remux tuner.HLSRemuxer) *tuner.M3UHost {
	t.Helper()
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: clock.NewFake(time.Unix(0, 0)), UserAgent: appUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	return tuner.NewM3UHost(client, clock.NewFake(time.Unix(0, 0)), remux)
}

// An HLS channel is fetched once through the polite client, then handed to
// the remuxer at its final URL with the tuner's or the application's
// User-Agent.
func TestM3UHostStreamRemuxesHLS(t *testing.T) {
	p := newPanel(t)
	streams := newStreamPanel(t)
	redirect := httptest.NewServer(http.RedirectHandler(streams.server.URL+"/hls/1.m3u8", http.StatusFound))
	t.Cleanup(redirect.Close)
	remux := &fakeRemuxer{}
	host := newHLSHost(t, remux)
	info := p.tuner()
	channel := tuner.Channel{ID: tuner.M3UChannelIDPrefixFor(info.Url) + "x", Path: redirect.URL + "/channel"}

	body, err := host.Stream(t.Context(), info, channel)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "remuxed" || len(remux.calls) != 1 || remux.calls[0] != streams.server.URL+"/hls/1.m3u8" || remux.agents[0] != appUserAgent {
		t.Fatalf("body %q, calls %v, agents %v", got, remux.calls, remux.agents)
	}

	info.UserAgent = "IPTVSmarters/1.0"
	channel.Path = streams.server.URL + "/hls/1.m3u8"
	if _, err := host.Stream(t.Context(), info, channel); err != nil {
		t.Fatal(err)
	}
	if remux.agents[1] != "IPTVSmarters/1.0" {
		t.Fatalf("agents %v", remux.agents)
	}
}

// A remux failure carries no credentials; a 401 on the playlist never
// starts the remuxer.
func TestM3UHostStreamHLSFailures(t *testing.T) {
	p := newPanel(t)
	streams := newStreamPanel(t)
	remux := &fakeRemuxer{err: errors.New("open " + streams.server.URL + "/live/alice/" + marker + "/1.m3u8 failed")}
	host := newHLSHost(t, remux)
	info := p.tuner()
	channel := tuner.Channel{ID: tuner.M3UChannelIDPrefixFor(info.Url) + "x", Path: streams.server.URL + "/live/alice/" + marker + "/1.m3u8"}

	_, err := host.Stream(t.Context(), info, channel)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatalf("remux failure: %v", err)
	}

	streams.mu.Lock()
	streams.status = http.StatusUnauthorized
	streams.mu.Unlock()
	if _, err := host.Stream(t.Context(), info, channel); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("401: %v", err)
	}
	if len(remux.calls) != 1 {
		t.Fatalf("remuxer started after a 401: %v", remux.calls)
	}
}
