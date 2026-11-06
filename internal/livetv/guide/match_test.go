// SPDX-License-Identifier: GPL-2.0-only

package guide_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// TestMatchOrder follows ListingsManager.GetEpgChannelFromTunerChannel
// (ListingsManager.cs:387-455).
func TestMatchOrder(t *testing.T) {
	index := guide.NewChannels([]guide.Channel{
		{ID: "by-id", Name: "Id Channel", Number: "1"},
		{ID: "tvg", Name: "Tvg Channel", Number: "2"},
		{ID: "12345", Name: "Schedules Direct", Number: "3"},
		{ID: "num", Name: "Number Channel", Number: "4.1"},
		{ID: "name", Name: "Name-Channel HD", Number: "5"},
		{ID: "mapped", Name: "Mapped", Number: "6"},
	})
	mappings := []config.NameValuePair{{Name: "TVG-OLD", Value: "mapped"}, {Name: "blank", Value: " "}}
	cases := []struct {
		name    string
		channel tuner.Channel
		want    string
	}{
		{"id first", tuner.Channel{ID: "BY-ID", TunerChannelID: "tvg", Number: "4.1"}, "by-id"},
		{"tvg-id", tuner.Channel{ID: "m3u_x", TunerChannelID: "TVG", Number: "4.1"}, "tvg"},
		{"schedules direct", tuner.Channel{TunerChannelID: "I12345.json.schedulesdirect.org"}, "12345"},
		{"mapped tvg-id", tuner.Channel{TunerChannelID: "tvg-old"}, "mapped"},
		{"blank mapping keeps the id", tuner.Channel{TunerChannelID: "blank", Number: "4.1"}, "num"},
		{"number", tuner.Channel{ID: "m3u_x", TunerChannelID: "unknown", Number: "4.1"}, "num"},
		{"normalized name", tuner.Channel{Number: "99", Name: "name channel-hd"}, "name"},
	}
	for _, tc := range cases {
		got, ok := index.Match(mappings, tc.channel)
		if !ok || got.ID != tc.want {
			t.Errorf("%s: got %q %v, want %q", tc.name, got.ID, ok, tc.want)
		}
	}
	if got, ok := index.Match(mappings, tuner.Channel{ID: "none", Name: "Nothing", Number: "77"}); ok {
		t.Errorf("unexpected match %+v", got)
	}
}

func TestEnabledForTuner(t *testing.T) {
	provider := config.ListingsProviderInfo{EnabledTuners: []string{"ABC"}}
	if !guide.EnabledForTuner(provider, "abc") || guide.EnabledForTuner(provider, "def") {
		t.Error("enabled tuners")
	}
	provider.EnableAllTuners = true
	if !guide.EnabledForTuner(provider, "def") {
		t.Error("all tuners")
	}
}

func TestRefreshWindow(t *testing.T) {
	now := time.Date(2026, 3, 8, 6, 30, 0, 0, time.FixedZone("", -5*3600))
	for _, tc := range []struct {
		days *int
		want int
	}{{nil, 7}, {intptr(0), 1}, {intptr(3), 3}, {intptr(30), 14}} {
		start, end := guide.RefreshWindow(now, tc.days)
		if !start.Equal(time.Date(2026, 3, 8, 10, 30, 0, 0, time.UTC)) || end.Sub(start) != time.Duration(tc.want)*24*time.Hour {
			t.Errorf("days %v: window %v to %v", tc.days, start, end)
		}
	}
}

// TestFakehdhrQuirksMatch matches the fakehdhr -quirks all playlist to its
// guide. The missing-ids entry has no tvg-id and matches by number.
func TestFakehdhrQuirksMatch(t *testing.T) {
	provider, _ := newProvider(t)
	channels, err := provider.Channels(context.Background(), providerInfo("quirks", "testdata/fakehdhr-quirks.xml"), "en")
	if err != nil {
		t.Fatal(err)
	}
	index := guide.NewChannels(channels)
	playlist, err := os.Open("testdata/fakehdhr-quirks.m3u")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = playlist.Close() }()
	matched, total := 0, 0
	err = tuner.ParseM3U(playlist, tuner.M3UChannelIDPrefixFor("http://127.0.0.1:15005/playlist.m3u"), "tuner", tuner.ParseOptions{}, func(channel tuner.Channel) error {
		total++
		found, ok := index.Match(nil, channel)
		if !ok {
			t.Errorf("no guide channel for %+v", channel)
			return nil
		}
		if channel.TunerChannelID == "" && found.ID != "fast-token.fake" {
			t.Errorf("missing-ids channel %q matched %q", channel.Name, found.ID)
		}
		matched++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 14 || matched != total {
		t.Errorf("matched %d of %d", matched, total)
	}
}
