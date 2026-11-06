// SPDX-License-Identifier: GPL-2.0-only

package playgrant_test

import (
	"sort"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/auth/playgrant"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestGrantChangesRestoreInTheNextStore (#620): the change feed carries
// issued and used grants and the removal of evicted or expired ones; the
// next store restores the live grants with their idle timers.
func TestGrantChangesRestoreInTheNextStore(t *testing.T) {
	fake := clock.NewFake(time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC))
	grants := playgrant.New(fake)
	grants.Issue("before", uuid(t, "00000000-0000-0000-0000-0000000000a9"), uuid(t, "00000000-0000-0000-0000-0000000000b9"), nil)
	notify := grants.EnablePersistence()
	if records, removed := grants.TakeChanges(); len(records) != 0 || len(removed) != 0 {
		t.Fatalf("changes before tracking = %v, %v", records, removed)
	}
	user := uuid(t, "00000000-0000-0000-0000-0000000000a1")
	item := uuid(t, "00000000-0000-0000-0000-0000000000b1")
	grants.Issue("play-1", user, item, []string{item.String()})
	grants.Issue("play-2", user, item, nil)
	select {
	case <-notify:
	default:
		t.Fatal("an issued grant did not signal")
	}
	records, removed := grants.TakeChanges()
	sort.Slice(records, func(i, j int) bool { return records[i].PlaySessionID < records[j].PlaySessionID })
	if len(records) != 2 || len(removed) != 0 || records[0].PlaySessionID != "play-1" || records[0].UserID != user ||
		records[0].ItemID != item || len(records[0].MediaSourceIDs) != 1 || !records[0].LastUsed.Equal(fake.Now()) {
		t.Fatalf("changes = %+v, removed %v", records, removed)
	}

	fake.Advance(time.Hour)
	if _, ok := grants.Authorize("play-1", item, ""); !ok {
		t.Fatal("play-1 does not authorize")
	}
	records, _ = grants.TakeChanges()
	if len(records) != 1 || !records[0].LastUsed.Equal(fake.Now()) {
		t.Fatalf("a used grant did not change: %+v", records)
	}
	persisted := append(records, store.PlayGrantRecord{PlaySessionID: "play-2", UserID: user, ItemID: item, LastUsed: fake.Now().Add(-time.Hour)})

	fake.Advance(playgrant.IdleTimeout - time.Minute)
	next := playgrant.New(fake)
	next.Restore(persisted)
	if got, ok := next.Authorize("play-1", item, item.String()); !ok || got != user {
		t.Fatalf("restored play-1 = %v, %v", got, ok)
	}
	if _, ok := next.Authorize("play-2", item, ""); ok {
		t.Fatal("a grant idle past IdleTimeout was restored")
	}

	// An expired grant leaves the store and the feed reports it, also one
	// issued before tracking started.
	fake.Advance(2 * time.Minute)
	grants.Issue("play-3", user, item, nil)
	records, removed = grants.TakeChanges()
	sort.Strings(removed)
	if len(records) != 1 || records[0].PlaySessionID != "play-3" || len(removed) != 3 || removed[0] != "before" || removed[1] != "play-1" || removed[2] != "play-2" {
		t.Fatalf("after expiry: records %+v, removed %v", records, removed)
	}
	grants.MarkDirty([]string{"play-3", "gone"})
	records, removed = grants.TakeChanges()
	if len(records) != 1 || len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("after MarkDirty: records %+v, removed %v", records, removed)
	}
}
