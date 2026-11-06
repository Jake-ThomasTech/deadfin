// SPDX-License-Identifier: GPL-2.0-only

package tuner_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

const (
	appUserAgent = "Deadfin/test (+https://github.com/deadfin-org/deadfin)"
	marker       = "MARKERpw-7b1e"
)

type panel struct {
	server     *httptest.Server
	requests   atomic.Int32
	status     atomic.Int32
	mu         sync.Mutex
	userAgents []string
	release    chan struct{}
	arrived    chan struct{}
}

func newPanel(t *testing.T) *panel {
	t.Helper()
	playlist, err := os.ReadFile("testdata/fakehdhr-playlist.m3u")
	if err != nil {
		t.Fatal(err)
	}
	p := &panel{}
	p.status.Store(http.StatusOK)
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.requests.Add(1)
		p.mu.Lock()
		p.userAgents = append(p.userAgents, r.UserAgent())
		release, arrived := p.release, p.arrived
		p.mu.Unlock()
		if release != nil {
			arrived <- struct{}{}
			<-release
		}
		if status := int(p.status.Load()); status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write(playlist)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *panel) tuner() config.TunerHostInfo {
	host := config.DefaultTunerHostInfo()
	host.Id = "0123456789abcdef0123456789abcdef"
	host.Type = "m3u"
	host.Url = p.server.URL + "/get.php?username=alice&password=" + marker + "&type=m3u_plus"
	return host
}

func newHost(t *testing.T, fake *clock.Fake) *tuner.M3UHost {
	t.Helper()
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: fake, UserAgent: appUserAgent})
	if err != nil {
		t.Fatal(err)
	}
	return tuner.NewM3UHost(client, fake, nil)
}

func TestM3UHostListsChannelsWithUserAgent(t *testing.T) {
	p := newPanel(t)
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	channels, err := host.Channels(t.Context(), info)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 23 {
		t.Fatalf("channels = %d, want 23", len(channels))
	}
	if want := tuner.M3UChannelIDPrefixFor(info.Url); !strings.HasPrefix(channels[0].ID, want) || channels[0].TunerHostID != info.Id {
		t.Fatalf("channel = %+v", channels[0])
	}

	override := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info.UserAgent = "IPTVSmarters/1.0"
	if _, err := override.Channels(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.userAgents, "|"); got != appUserAgent+"|IPTVSmarters/1.0" {
		t.Fatalf("User-Agents = %q", got)
	}
}

func TestM3UHostDownloadsAtMostOncePerInterval(t *testing.T) {
	p := newPanel(t)
	fake := clock.NewFake(time.Unix(0, 0))
	host := newHost(t, fake)
	info := p.tuner()
	for range 3 {
		if _, err := host.Channels(t.Context(), info); err != nil {
			t.Fatal(err)
		}
	}
	fake.Advance(tuner.MinRefreshInterval - time.Second)
	if _, err := host.Channels(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	if got := p.requests.Load(); got != 1 {
		t.Fatalf("requests inside the interval = %d, want 1", got)
	}
	fake.Advance(time.Second)
	if _, err := host.Channels(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	if got := p.requests.Load(); got != 2 {
		t.Fatalf("requests after the interval = %d, want 2", got)
	}
	host.Forget(info)
	if _, err := host.Channels(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	if got := p.requests.Load(); got != 3 {
		t.Fatalf("requests after Forget = %d, want 3", got)
	}
}

func TestM3UHostSharesConcurrentDownloads(t *testing.T) {
	p := newPanel(t)
	p.release = make(chan struct{})
	p.arrived = make(chan struct{}, 4)
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := host.Channels(t.Context(), info); err != nil {
				t.Error(err)
			}
		})
	}
	<-p.arrived
	close(p.release)
	wg.Wait()
	if got := p.requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 shared download", got)
	}
}

// L14 import side and L16 a: a rejected password costs one request, the
// last good list survives, and the marker reaches no error or log line.
func TestM3UHostRejectedCredentials(t *testing.T) {
	p := newPanel(t)
	fake := clock.NewFake(time.Unix(0, 0))
	host := newHost(t, fake)
	info := p.tuner()
	if _, err := host.Channels(t.Context(), info); err != nil {
		t.Fatal(err)
	}
	p.status.Store(http.StatusUnauthorized)

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	for range 3 {
		fake.Advance(tuner.MinRefreshInterval)
		channels, err := host.Channels(t.Context(), info)
		if !errors.Is(err, fetch.ErrCredentialsRejected) {
			t.Fatalf("Channels = %v, want ErrCredentialsRejected", err)
		}
		if len(channels) != 23 {
			t.Fatalf("last good channels = %d, want 23", len(channels))
		}
		logger.Error("refresh tuner", "error", err)
	}
	if got := p.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (one good, one 401)", got)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("log leaks the password:\n%s", logs.String())
	}
}

func TestM3UHostReadsLocalFile(t *testing.T) {
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := config.DefaultTunerHostInfo()
	info.Id = "local"
	info.Url = "testdata/fakehdhr-lineup.m3u"
	channels, err := host.Channels(t.Context(), info)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 23 {
		t.Fatalf("channels = %d, want 23", len(channels))
	}
	info.Url = ""
	if _, err := host.Channels(t.Context(), info); !errors.Is(err, tuner.ErrNoTunerURL) {
		t.Fatalf("empty Url: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	registry := tuner.NewRegistry(host)
	if got, ok := registry.Host("M3U"); !ok || got != host {
		t.Fatalf("Host(M3U) = %v, %v", got, ok)
	}
	if _, ok := registry.Host("hdhomerun"); ok {
		t.Fatal("Host(hdhomerun) found an unregistered type")
	}
	if got := registry.Types(); len(got) != 1 || got[0] != (tuner.TypeInfo{Name: "M3U Tuner", ID: "m3u"}) {
		t.Fatalf("Types = %+v", got)
	}
}

// The Deadfin-only tuner fields (D-78): PlaylistRefreshMinutes lengthens
// the download interval but never shortens it below the floor, and
// SkipVodEntries drops the Xtream movie and series entries.
func TestM3UHostDeadfinTunerFields(t *testing.T) {
	info := config.DefaultTunerHostInfo()
	if tuner.RefreshInterval(info) != tuner.MinRefreshInterval {
		t.Fatal("unset interval is not the floor")
	}
	minutes := 5
	info.PlaylistRefreshMinutes = &minutes
	if tuner.RefreshInterval(info) != tuner.MinRefreshInterval {
		t.Fatal("interval went under the floor")
	}
	minutes = 120
	if tuner.RefreshInterval(info) != 2*time.Hour {
		t.Fatal("interval not applied")
	}

	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info.Id, info.Type, info.Url = "xtream", "m3u", "testdata/fakehdhr-xtream.m3u"
	all, err := host.Channels(t.Context(), info)
	if err != nil {
		t.Fatal(err)
	}
	skip := true
	info.SkipVodEntries = &skip
	host.Forget(info)
	live, err := host.Channels(t.Context(), info)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) == 0 || len(live) >= len(all) {
		t.Fatalf("SkipVodEntries kept %d of %d entries", len(live), len(all))
	}
	for _, channel := range live {
		if !strings.Contains(channel.Path, "/live/") {
			t.Fatalf("VOD entry kept: %s", channel.Path)
		}
	}
}

// Forget is the administrator's retry: it clears a rejected source.
func TestM3UHostForgetResetsRejectedSource(t *testing.T) {
	p := newPanel(t)
	host := newHost(t, clock.NewFake(time.Unix(0, 0)))
	info := p.tuner()
	p.status.Store(http.StatusUnauthorized)
	if _, err := host.Channels(t.Context(), info); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("first error = %v", err)
	}
	p.status.Store(http.StatusOK)
	if _, err := host.Channels(t.Context(), info); !errors.Is(err, fetch.ErrCredentialsRejected) {
		t.Fatalf("rejected source sent a request: %v", err)
	}
	host.Forget(info)
	if _, err := host.Channels(t.Context(), info); err != nil {
		t.Fatalf("after Forget: %v", err)
	}
	if got := p.requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestStreamOpenInterval(t *testing.T) {
	ms := func(v int) *int { return &v }
	for _, tc := range []struct {
		value *int
		want  time.Duration
	}{
		{nil, time.Second},
		{ms(250), 250 * time.Millisecond},
		{ms(0), 0},
		{ms(-5), 0},
	} {
		if got := tuner.StreamOpenInterval(config.TunerHostInfo{StreamOpenIntervalMs: tc.value}); got != tc.want {
			t.Errorf("StreamOpenIntervalMs %v: %v, want %v", tc.value, got, tc.want)
		}
	}
}
