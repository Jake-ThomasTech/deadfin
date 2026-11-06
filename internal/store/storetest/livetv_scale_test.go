// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// TestLiveTvImportWriterLatency imports DEADFIN_LIVETV_SCALE channels of 14
// days of 30-minute programmes into file-backed SQLite, one channel per
// transaction, while a playstate write runs every 20 ms. It fails when a
// playstate write waits a second or more. It is a manual scale check: unset,
// it skips.
func TestLiveTvImportWriterLatency(t *testing.T) {
	scale, _ := strconv.Atoi(os.Getenv("DEADFIN_LIVETV_SCALE"))
	if scale <= 0 {
		t.Skip("DEADFIN_LIVETV_SCALE is not set")
	}
	ctx := t.Context()
	stores, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "deadfin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	items := store.NewItemStore(stores)
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	folder := catalog.Folder("/data/metadata", "/data", now)
	if err := items.Upsert(ctx, folder); err != nil {
		t.Fatal(err)
	}
	movie := item.Item{ID: dto.UUID{0xfe}, Kind: item.KindMovie, Name: stringPtr("Movie")}
	if err := items.Upsert(ctx, movie); err != nil {
		t.Fatal(err)
	}
	channels := make([]item.Item, scale)
	for index := range channels {
		id := "m3u_" + strconv.Itoa(index)
		channels[index] = catalog.Channel(catalog.ChannelInfo{ID: id, Name: id, Number: strconv.Itoa(index + 1)}, folder.ID, nil, now)
	}
	if _, err := items.ReconcileChildren(ctx, folder.ID, channels); err != nil {
		t.Fatal(err)
	}

	// The latency is wall time by intent; the real clock measures it.
	wall := clock.NewReal()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var worst time.Duration
	writes := 0
	wg.Add(1)
	go func() {
		defer wg.Done()
		user := dto.UUID{0xaa}
		ticker := wall.NewTimer(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C():
				ticker.Reset(20 * time.Millisecond)
			}
			begin := wall.Now()
			err := items.UpsertUserData(context.WithoutCancel(ctx), store.UserData{UserID: user, CustomKey: movie.ID.String(), ItemID: movie.ID, PositionTicks: int64(writes)})
			if err != nil {
				t.Error(err)
				return
			}
			worst = max(worst, wall.Now().Sub(begin))
			writes++
		}
	}()

	begin := wall.Now()
	programmes := 0
	start := now.Add(-time.Hour)
	for _, channel := range channels {
		desired := make([]item.Item, 0, 14*48)
		for slot := range 14 * 48 {
			at := start.Add(time.Duration(slot) * 30 * time.Minute)
			title := "Programme " + strconv.Itoa(slot%37)
			p := guide.Program{ID: *channel.ExternalID + "_" + at.Format(time.RFC3339), ChannelID: *channel.ExternalID, Name: &title, StartDate: at, EndDate: at.Add(30 * time.Minute), Genres: []string{"news"}, IsNews: true}
			desired = append(desired, catalog.Program(p, channel, nil, now))
		}
		if _, err := items.ReconcileChildren(ctx, channel.ID, desired); err != nil {
			t.Fatal(err)
		}
		programmes += len(desired)
	}
	elapsed := wall.Now().Sub(begin)
	close(stop)
	wg.Wait()
	t.Logf("%d channels, %d programmes in %v (%.0f/s); %d playstate writes, worst wait %v",
		scale, programmes, elapsed, float64(programmes)/elapsed.Seconds(), writes, worst)
	if worst >= time.Second {
		t.Errorf("a playstate write waited %v", worst)
	}
}
