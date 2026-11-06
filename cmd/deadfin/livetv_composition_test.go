// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/tasks"
)

// TestLiveTvAdminFillsGuide is the P5 acceptance path of #541: an
// administrator adds a tuner and a guide through the stock jellyfin-web
// calls, each save queues Refresh Guide, and the refresh stores the channels
// and programmes. The tuner is the checked-in fakehdhr playlist.
func TestLiveTvAdminFillsGuide(t *testing.T) {
	app, token := newLibraryRefreshTestApp(t)
	items := store.NewItemStore(app.stores)

	info, ok := app.scheduler.Get(tasks.RefreshGuideID)
	if !ok || !info.IsHidden {
		t.Fatalf("Refresh Guide task with no tuner = %+v, %t; want hidden", info, ok)
	}
	if kinds := liveTvKindCounts(t, items); kinds[item.KindLiveTvChannel]+kinds[item.KindLiveTvProgram] != 0 {
		t.Fatalf("Live TV rows before any tuner: %v", kinds)
	}

	testdata, err := filepath.Abs(filepath.Join("..", "..", "internal", "livetv", "guide", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tunerBody := fmt.Sprintf(`{"Type":"m3u","Url":%q,"FriendlyName":"fakehdhr"}`, filepath.Join(testdata, "fakehdhr-quirks.m3u"))
	rec := authenticatedCompositionRequest(app.Router(), http.MethodPost, "/LiveTv/TunerHosts", []byte(tunerBody), token)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /LiveTv/TunerHosts = %d: %s", rec.Code, rec.Body.String())
	}
	var tuner struct{ Id string }
	if err := json.Unmarshal(rec.Body.Bytes(), &tuner); err != nil || len(tuner.Id) != 32 {
		t.Fatalf("saved tuner %s: %v", rec.Body.String(), err)
	}
	guidePath := writeCurrentGuide(t, clock.NewReal().Now().UTC())
	guideBody := fmt.Sprintf(`{"Type":"xmltv","Path":%q,"EnableAllTuners":true}`, guidePath)
	rec = authenticatedCompositionRequest(app.Router(), http.MethodPost, "/LiveTv/ListingProviders", []byte(guideBody), token)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /LiveTv/ListingProviders = %d: %s", rec.Code, rec.Body.String())
	}
	if info, _ := app.scheduler.Get(tasks.RefreshGuideID); info.IsHidden {
		t.Fatal("Refresh Guide task hidden with a tuner")
	}

	// The second save cancels the first run and queues another; wait for a
	// run that completes and has stored the guide.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		info, _ := app.scheduler.Get(tasks.RefreshGuideID)
		if info.State == tasks.StateIdle && info.LastExecutionResult != nil && info.LastExecutionResult.Status == tasks.StatusCompleted {
			if kinds := liveTvKindCounts(t, items); kinds[item.KindLiveTvChannel] == 14 && kinds[item.KindLiveTvProgram] == 6 {
				assertChannelPlaceholderSource(t, app, token)
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("guide not stored: task %+v, rows %v", info, liveTvKindCounts(t, items))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// assertChannelPlaceholderSource checks a channel item's media source as
// LiveTvChannel.GetMediaSources builds it: an infinite Http placeholder with
// no path (D-100).
func assertChannelPlaceholderSource(t *testing.T, app *application, token string) {
	t.Helper()
	rec := authenticatedCompositionRequest(app.Router(), http.MethodGet, "/LiveTv/Channels?limit=1", nil, token)
	var channels struct{ Items []struct{ Id string } }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &channels) != nil || len(channels.Items) != 1 {
		t.Fatalf("GET /LiveTv/Channels = %d: %s", rec.Code, rec.Body.String())
	}
	rec = authenticatedCompositionRequest(app.Router(), http.MethodGet, "/LiveTv/Channels/"+channels.Items[0].Id, nil, token)
	var channel struct {
		MediaSources []struct {
			Protocol, Type, Path string
			IsInfiniteStream     bool
		}
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &channel) != nil || len(channel.MediaSources) != 1 {
		t.Fatalf("GET /LiveTv/Channels/{id} = %d: %s", rec.Code, rec.Body.String())
	}
	if source := channel.MediaSources[0]; source.Protocol != "Http" || source.Type != "Placeholder" || !source.IsInfiniteStream || source.Path != "" {
		t.Fatalf("channel media source %+v", source)
	}
}

// writeCurrentGuide writes an XMLTV guide with three programmes on each of
// two fakehdhr channels, starting half an hour before now. The checked-in
// fakehdhr guide is anchored in March 2026, outside a real-clock window.
func writeCurrentGuide(t *testing.T, now time.Time) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n<tv>\n")
	for _, channel := range []string{"fake-atsc.fake", "fake-news.fake"} {
		fmt.Fprintf(&b, "<channel id=%q><display-name>%s</display-name></channel>\n", channel, channel)
	}
	start := now.Add(-30 * time.Minute).Truncate(time.Minute)
	for _, channel := range []string{"fake-atsc.fake", "fake-news.fake"} {
		for index := range 3 {
			from := start.Add(time.Duration(index) * time.Hour)
			fmt.Fprintf(&b, "<programme start=%q stop=%q channel=%q><title>Show %d</title></programme>\n",
				from.Format("20060102150405 -0700"), from.Add(time.Hour).Format("20060102150405 -0700"), channel, index)
		}
	}
	b.WriteString("</tv>\n")
	path := filepath.Join(t.TempDir(), "guide.xml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func liveTvKindCounts(t *testing.T, items *store.ItemStore) map[item.Kind]int {
	t.Helper()
	out := make(map[item.Kind]int)
	one := 1
	for _, kind := range []item.Kind{item.KindLiveTvChannel, item.KindLiveTvProgram} {
		result, err := items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{kind}, EnableTotalRecordCount: true, Limit: &one})
		if err != nil {
			t.Fatal(err)
		}
		out[kind] = result.TotalRecordCount
	}
	return out
}
