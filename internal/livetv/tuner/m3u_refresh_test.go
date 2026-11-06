// SPDX-License-Identifier: GPL-2.0-only

package tuner_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// tokenPanel lists one channel whose URL carries a token. Each playlist
// download issues the current token; a stream with any other token gets
// expiredStatus, as an expiring CDN token does (fakehdhr 800.4).
type tokenPanel struct {
	server        *httptest.Server
	token         atomic.Int32
	playlists     atomic.Int32
	expiredStatus int
	streamStatus  atomic.Int32
	mu            sync.Mutex
	streams       []string
}

func newTokenPanel(t *testing.T, expiredStatus int) *tokenPanel {
	t.Helper()
	p := &tokenPanel{expiredStatus: expiredStatus}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/get.php" {
			p.playlists.Add(1)
			fmt.Fprintf(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"news.us\" tvg-chno=\"5\",News\n%s/live/alice/%s/5.ts?token=%d\n", p.server.URL, marker, p.token.Load())
			return
		}
		p.mu.Lock()
		p.streams = append(p.streams, r.URL.RawQuery)
		p.mu.Unlock()
		if status := int(p.streamStatus.Load()); status != 0 {
			w.WriteHeader(status)
			return
		}
		if r.URL.Query().Get("token") != fmt.Sprint(p.token.Load()) {
			w.WriteHeader(p.expiredStatus)
			return
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = io.WriteString(w, "tsbytes")
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *tokenPanel) tuner() config.TunerHostInfo {
	host := config.DefaultTunerHostInfo()
	host.Id = "fedcba9876543210fedcba9876543210"
	host.Type = "m3u"
	host.Url = p.server.URL + "/get.php?username=alice&password=" + marker
	return host
}

func firstChannel(t *testing.T, host *tuner.M3UHost, info config.TunerHostInfo) tuner.Channel {
	t.Helper()
	channels, err := host.Channels(t.Context(), info)
	if err != nil || len(channels) != 1 {
		t.Fatalf("channels %v %v", channels, err)
	}
	return channels[0]
}

func readStream(t *testing.T, host *tuner.M3UHost, info config.TunerHostInfo, channel tuner.Channel) error {
	t.Helper()
	body, err := host.Stream(t.Context(), info, channel)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	got, _ := io.ReadAll(body)
	if string(got) != "tsbytes" {
		t.Fatalf("stream %q", got)
	}
	return nil
}

// An expired channel URL re-reads the playlist once and plays at the new
// URL; later opens of the stored channel use the new URL directly.
func TestM3UHostStreamRenewsExpiredURL(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			p := newTokenPanel(t, status)
			fake := clock.NewFake(time.Unix(0, 0))
			host := newHostWithClock(t, fake)
			info := p.tuner()
			channel := firstChannel(t, host, info)

			p.token.Store(1)
			if err := readStream(t, host, info, channel); err != nil {
				t.Fatalf("renewed stream: %v", err)
			}
			if p.playlists.Load() != 2 {
				t.Fatalf("playlist downloads %d, want 2", p.playlists.Load())
			}
			if err := readStream(t, host, info, channel); err != nil {
				t.Fatalf("second open: %v", err)
			}
			if got := p.streamQueries(); strings.Join(got, ",") != "token=0,token=1,token=1" {
				t.Fatalf("stream requests %v", got)
			}
			if again := firstChannel(t, host, info); again.ID != channel.ID {
				t.Fatalf("the re-read replaced the cached channel id: %s, was %s", again.ID, channel.ID)
			}
		})
	}
}

// Inside StreamRefreshInterval a second expiry does not re-read the
// playlist; after it, it does.
func TestM3UHostStreamRenewalIsRateLimited(t *testing.T) {
	p := newTokenPanel(t, http.StatusForbidden)
	fake := clock.NewFake(time.Unix(0, 0))
	host := newHostWithClock(t, fake)
	info := p.tuner()
	channel := firstChannel(t, host, info)

	p.token.Store(1)
	if err := readStream(t, host, info, channel); err != nil {
		t.Fatal(err)
	}
	p.token.Store(2)
	var status *fetch.StatusError
	if err := readStream(t, host, info, channel); !errors.As(err, &status) || status.StatusCode != http.StatusForbidden || strings.Contains(err.Error(), marker) {
		t.Fatalf("expiry inside the interval: %v", err)
	}
	if p.playlists.Load() != 2 {
		t.Fatalf("playlist downloads %d, want 2", p.playlists.Load())
	}
	fake.Advance(tuner.StreamRefreshInterval)
	if err := readStream(t, host, info, channel); err != nil {
		t.Fatalf("expiry after the interval: %v", err)
	}
	if p.playlists.Load() != 3 {
		t.Fatalf("playlist downloads %d, want 3", p.playlists.Load())
	}
}

// Concurrent opens of an expired channel share one playlist re-read.
func TestM3UHostStreamRenewalIsSingleFlight(t *testing.T) {
	p := newTokenPanel(t, http.StatusGone)
	host := newHostWithClock(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	channel := firstChannel(t, host, info)
	p.token.Store(1)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, err := host.Stream(t.Context(), info, channel)
			if err == nil {
				_ = body.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.playlists.Load() != 2 {
		t.Fatalf("playlist downloads %d, want 2", p.playlists.Load())
	}
}

// A 401 on a stream does not re-read the playlist; it rejects the tuner.
func TestM3UHostStream401RejectsWithoutRenewal(t *testing.T) {
	p := newTokenPanel(t, http.StatusForbidden)
	host := newHostWithClock(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	channel := firstChannel(t, host, info)
	p.streamStatus.Store(http.StatusUnauthorized)

	if err := readStream(t, host, info, channel); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("401: %v", err)
	}
	if p.playlists.Load() != 1 {
		t.Fatalf("playlist re-read after a 401: %d", p.playlists.Load())
	}
	if err := readStream(t, host, info, channel); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("second stream after a 401: %v", err)
	}
	if got := p.streamQueries(); len(got) != 1 {
		t.Fatalf("stream requests after a 401: %v", got)
	}
}

func (p *tokenPanel) streamQueries() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.streams...)
}

func newHostWithClock(t *testing.T, fake *clock.Fake) *tuner.M3UHost {
	t.Helper()
	return newHost(t, fake)
}
