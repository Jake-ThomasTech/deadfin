// SPDX-License-Identifier: GPL-2.0-only

package itemstore_test

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/fetch"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/itemstore"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

type staticConfig struct {
	mu    sync.Mutex
	value config.LiveTvOptions
}

func (c *staticConfig) LiveTv() config.LiveTvOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (c *staticConfig) UpdateLiveTv(_ context.Context, update func(*config.LiveTvOptions) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return update(&c.value)
}

// TestRefreshGuideOnSQLite runs Refresh Guide on the fakehdhr fixtures into
// a SQLite store and reads the guide grid back through ItemQuery.
func TestRefreshGuideOnSQLite(t *testing.T) {
	ctx := t.Context()
	stores, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	items := store.NewItemStore(stores)

	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	client, err := fetch.New(fetch.Options{HTTP: &http.Client{}, Clock: clk, UserAgent: "Deadfin-test"})
	if err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs(filepath.Join("..", "guide", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	options := config.DefaultLiveTvOptions()
	days := 1
	options.GuideDays = &days
	host := config.DefaultTunerHostInfo()
	host.Id, host.Type, host.Url = "tuner1", "m3u", filepath.Join(testdata, "fakehdhr-quirks.m3u")
	provider := config.DefaultListingsProviderInfo()
	provider.Id, provider.Type, provider.Path = "guide1", "xmltv", filepath.Join(testdata, "fakehdhr-quirks.xml")
	options.TunerHosts = []config.TunerHostInfo{host}
	options.ListingProviders = []config.ListingsProviderInfo{provider}

	service, err := livetv.New(livetv.Options{
		Config:   &staticConfig{value: options},
		Tuners:   tuner.NewRegistry(tuner.NewM3UHost(client, clk, nil)),
		Listings: []livetv.ListingsProvider{guide.NewXMLTV(client, clk, t.TempDir())},
		Items:    itemstore.New(items),
		Clock:    clk,
		Logger:   slog.New(slog.DiscardHandler),
		Paths:    livetv.Paths{ProgramData: "/data", InternalMetadata: "/data/metadata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.RefreshGuide(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Channels != 14 || first.Programs == 0 || first.Inserted != first.Channels+first.Programs {
		t.Fatalf("first refresh %+v", first)
	}
	again, err := service.RefreshGuide(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changes != (livetv.Changes{}) {
		t.Fatalf("unchanged refresh wrote %+v", again.Changes)
	}

	folder := catalog.Folder("/data/metadata", "/data", now)
	start, end := guide.RefreshWindow(now, &days)
	grid, err := items.Query(ctx, store.ItemQuery{
		IncludeItemTypes:       []item.Kind{item.KindLiveTvProgram},
		TopParentIDs:           []dto.UUID{folder.ID},
		MinEndDate:             &start,
		MaxStartDate:           &end,
		EnableTotalRecordCount: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if grid.TotalRecordCount != first.Programs {
		t.Fatalf("grid has %d programmes, refresh stored %d", grid.TotalRecordCount, first.Programs)
	}
}

// MergeChildren writes the listed children and keeps the omitted ones;
// ReplaceChildren then deletes them.
func TestMergeChildrenKeepsOmittedChildren(t *testing.T) {
	ctx := t.Context()
	stores, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	items := itemstore.New(store.NewItemStore(stores))
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	folder := catalog.Folder("/data/metadata", "/data", now)
	if err := items.Upsert(ctx, folder); err != nil {
		t.Fatal(err)
	}
	channel := func(id string) item.Item {
		return catalog.Channel(catalog.ChannelInfo{ID: id, Name: id, ChannelType: item.ChannelTypeTV}, folder.ID, nil, now)
	}
	a, b := channel("m3u_a"), channel("m3u_b")
	if changes, err := items.MergeChildren(ctx, folder.ID, []item.Item{a}); err != nil || changes.Inserted != 1 {
		t.Fatalf("merge a: %+v %v", changes, err)
	}
	if changes, err := items.MergeChildren(ctx, folder.ID, []item.Item{b}); err != nil || changes.Inserted != 1 || changes.Deleted != 0 {
		t.Fatalf("merge b: %+v %v", changes, err)
	}
	children, err := items.Children(ctx, folder.ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("children after merges: %d %v", len(children), err)
	}
	if changes, err := items.ReplaceChildren(ctx, folder.ID, []item.Item{b}); err != nil || changes.Deleted != 1 || changes.Inserted != 0 || changes.Updated != 0 {
		t.Fatalf("replace: %+v %v", changes, err)
	}
}
