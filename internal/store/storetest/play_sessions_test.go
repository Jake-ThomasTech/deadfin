// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// TestPlaySessionStore defends the play session and grant tables (#620) on
// SQLite and, when DEADFIN_TEST_POSTGRES_DSN is set, PostgreSQL.
func TestPlaySessionStore(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	t.Run("sqlite", func(t *testing.T) { verifyPlaySessionStore(t, stores) })
	if dsn := os.Getenv("DEADFIN_TEST_POSTGRES_DSN"); dsn != "" {
		pg := openPostgresFromDSN(t, dsn)
		t.Run("postgres", func(t *testing.T) { verifyPlaySessionStore(t, pg) })
	}
}

func verifyPlaySessionStore(t *testing.T, stores store.Stores) {
	ctx := context.Background()
	sessions := store.NewPlaySessionStore(stores)
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	user := dto.UUID{0xa1, 0x02}
	item := dto.UUID{0xb1, 0x03}

	if err := sessions.SavePlaySessions(ctx, store.PlaySessionChanges{}); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if err := sessions.SavePlaySessions(ctx, store.PlaySessionChanges{
		Sessions: []store.PlaySessionRecord{
			{ID: "old", UserID: user, LastActivity: now.Add(-48 * time.Hour), State: []byte(`{"v":1}`)},
			{ID: "live", UserID: user, LastActivity: now, State: []byte(`{"v":1,"a":1}`)},
		},
		Grants: []store.PlayGrantRecord{
			{PlaySessionID: "grant-old", UserID: user, ItemID: item, LastUsed: now.Add(-5 * time.Hour)},
			{PlaySessionID: "grant-live", UserID: user, ItemID: item, MediaSourceIDs: []string{"b103", "alt"}, LastUsed: now},
			{PlaySessionID: "grant-gone", UserID: user, ItemID: item, LastUsed: now},
		},
	}); err != nil {
		t.Fatalf("SavePlaySessions: %v", err)
	}
	// A second write replaces a record, and removes others.
	if err := sessions.SavePlaySessions(ctx, store.PlaySessionChanges{
		Sessions:        []store.PlaySessionRecord{{ID: "live", UserID: user, LastActivity: now.Add(time.Minute), State: []byte(`{"v":1,"a":2}`)}},
		RemovedSessions: []string{"missing"},
		RemovedGrants:   []string{"grant-gone"},
	}); err != nil {
		t.Fatalf("second SavePlaySessions: %v", err)
	}

	got, err := sessions.PlaySessions(ctx, now.Add(-24*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("PlaySessions = %+v, %v", got, err)
	}
	if got[0].ID != "live" || got[0].UserID != user || !got[0].LastActivity.Equal(now.Add(time.Minute)) || string(got[0].State) != `{"v":1,"a":2}` {
		t.Fatalf("live session = %+v", got[0])
	}
	all, err := sessions.PlaySessions(ctx, time.Time{})
	if err != nil || len(all) != 2 || all[0].ID != "old" {
		t.Fatalf("all sessions = %+v, %v", all, err)
	}
	grants, err := sessions.PlayGrants(ctx, now.Add(-4*time.Hour))
	if err != nil || len(grants) != 1 {
		t.Fatalf("PlayGrants = %+v, %v", grants, err)
	}
	want := store.PlayGrantRecord{PlaySessionID: "grant-live", UserID: user, ItemID: item, MediaSourceIDs: []string{"b103", "alt"}}
	if grant := grants[0]; !grant.LastUsed.Equal(now) || grant.PlaySessionID != want.PlaySessionID || grant.UserID != want.UserID ||
		grant.ItemID != want.ItemID || !reflect.DeepEqual(grant.MediaSourceIDs, want.MediaSourceIDs) {
		t.Fatalf("live grant = %+v", grant)
	}

	if err := sessions.DeleteStalePlaySessions(ctx, now.Add(-24*time.Hour), now.Add(-4*time.Hour)); err != nil {
		t.Fatalf("DeleteStalePlaySessions: %v", err)
	}
	if all, err := sessions.PlaySessions(ctx, time.Time{}); err != nil || len(all) != 1 || all[0].ID != "live" {
		t.Fatalf("sessions after stale delete = %+v, %v", all, err)
	}
	if all, err := sessions.PlayGrants(ctx, time.Time{}); err != nil || len(all) != 1 || all[0].PlaySessionID != "grant-live" {
		t.Fatalf("grants after stale delete = %+v, %v", all, err)
	}
	if err := sessions.SavePlaySessions(ctx, store.PlaySessionChanges{RemovedSessions: []string{"live"}, RemovedGrants: []string{"grant-live"}}); err != nil {
		t.Fatal(err)
	}
	if all, err := sessions.PlaySessions(ctx, time.Time{}); err != nil || len(all) != 0 {
		t.Fatalf("sessions after removal = %+v, %v", all, err)
	}
}
