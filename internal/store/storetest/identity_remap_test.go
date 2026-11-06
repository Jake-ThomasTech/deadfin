// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

func remapBackends() []struct {
	name string
	open func(*testing.T) store.Stores
} {
	return []struct {
		name string
		open func(*testing.T) store.Stores
	}{
		{name: "sqlite", open: openRemapSQLite},
		{name: "postgres", open: storetest.OpenPostgres},
	}
}

func openRemapSQLite(t *testing.T) store.Stores {
	t.Helper()
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := stores.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return stores
}

// TestIdentityRemapUserIDsRewritesEveryReference pins RemapUserIDs: every
// user gets a new id, every reference moves with it, row counts stay the
// same, access tokens still authenticate as the same user, and a second run
// changes nothing.
func TestIdentityRemapUserIDsRewritesEveryReference(t *testing.T) {
	for _, backend := range remapBackends() {
		t.Run(backend.name, func(t *testing.T) {
			stores := backend.open(t)
			identity, items := store.NewIdentity(stores), store.NewItemStore(stores)
			ctx := t.Context()
			now := time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC)

			nana, kai := contractUUID(t, 42001), contractUUID(t, 42002)
			for _, user := range []store.User{
				{ID: nana, Name: "nana", NormalizedName: "NANA", PasswordHash: "$PBKDF2-SHA512$iterations=1$AA$BB", Policy: []byte(`{"IsAdministrator":true}`), LastLoginDate: &now},
				{ID: kai, Name: "kai", NormalizedName: "KAI"},
			} {
				if _, err := identity.CreateUser(ctx, user); err != nil {
					t.Fatalf("create user %q: %v", user.Name, err)
				}
			}
			for user, token := range map[dto.UUID]string{nana: "nana-token", kai: "kai-token"} {
				if _, err := identity.CreateDevice(ctx, store.Device{UserID: user, AccessToken: token, AppName: "Swiftfin", AppVersion: "1", DeviceName: "phone", DeviceID: token + "-device", DateCreated: now, DateModified: now, DateLastActivity: now}); err != nil {
					t.Fatalf("create device %q: %v", token, err)
				}
			}
			folder := hierarchyItem(t, 42010, item.KindPlaylistsFolder, nil)
			movie := hierarchyItem(t, 42011, item.KindMovie, nil)
			for _, value := range []item.Item{folder, movie} {
				if err := items.Upsert(ctx, value); err != nil {
					t.Fatalf("seed %s: %v", value.ID, err)
				}
			}
			if err := identity.PutDisplayPreference(ctx, store.DisplayPreference{UserID: nana, ItemID: movie.ID, Client: "emby", Data: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
			value := "x"
			if err := identity.PutCustomItemDisplayPreference(ctx, store.CustomItemDisplayPreference{UserID: nana, ItemID: movie.ID, Client: "emby", Key: "k", Value: &value}); err != nil {
				t.Fatal(err)
			}
			if err := identity.ReplaceAccessSchedules(ctx, kai, []store.AccessSchedule{{DayOfWeek: 1, StartHour: 8, EndHour: 20}}); err != nil {
				t.Fatal(err)
			}
			for _, user := range []dto.UUID{nana, kai} {
				if _, err := items.MutateUserDataForItem(ctx, user, movie.ID, func(data *store.UserData) error {
					data.Played, data.PlayCount = true, 3
					return nil
				}); err != nil {
					t.Fatalf("seed user data: %v", err)
				}
			}
			playlist := hierarchyItem(t, 42020, item.KindPlaylist, &folder.ID)
			playlist.PlaylistOwnerUserID = nana
			playlist.IsFolder = true
			if err := items.CreatePlaylist(ctx, playlist, []store.ItemShare{{UserID: kai, CanEdit: true}}); err != nil {
				t.Fatalf("create playlist: %v", err)
			}
			if err := store.NewPlaySessionStore(stores).SavePlaySessions(ctx, store.PlaySessionChanges{
				Sessions: []store.PlaySessionRecord{{ID: "nana-session", UserID: nana, LastActivity: now, State: []byte(`{}`)}},
				Grants:   []store.PlayGrantRecord{{PlaySessionID: "kai-grant", UserID: kai, ItemID: movie.ID, LastUsed: now}},
			}); err != nil {
				t.Fatalf("seed play sessions: %v", err)
			}

			if _, err := store.NewActivityLogStore(stores).Insert(ctx, store.ActivityLogEntry{Name: "nana is online from phone", Type: "SessionStarted", UserID: nana, DateCreated: now, Severity: "Information"}); err != nil {
				t.Fatalf("seed activity log: %v", err)
			}

			before := map[dto.UUID]map[string]int{nana: countUserReferences(t, stores, nana), kai: countUserReferences(t, stores, kai)}
			for _, ref := range store.UserIDReferences() {
				if before[nana][ref.String()]+before[kai][ref.String()] == 0 {
					t.Fatalf("fixture has no row in %s; the test would not prove its rewrite", ref)
				}
			}

			fresh := map[dto.UUID]dto.UUID{}
			minted := []dto.UUID{contractUUID(t, 42101), contractUUID(t, 42102)}
			mint := func() (dto.UUID, error) {
				id := minted[0]
				minted = minted[1:]
				return id, nil
			}

			// A dry run counts the rows and writes nothing.
			dry, err := identity.RemapUserIDs(ctx, mint, now, true)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if len(dry.Users) != 2 || dry.AlreadyRemapped {
				t.Fatalf("dry report = %+v", dry)
			}
			if got, err := identity.User(ctx, nana); err != nil || got.Name != "nana" {
				t.Fatalf("dry run changed users: %+v, %v", got, err)
			}

			minted = []dto.UUID{contractUUID(t, 42101), contractUUID(t, 42102)}
			report, err := identity.RemapUserIDs(ctx, mint, now, false)
			if err != nil {
				t.Fatalf("remap: %v", err)
			}
			if report.AlreadyRemapped || len(report.Users) != 2 {
				t.Fatalf("report = %+v", report)
			}
			for _, change := range report.Users {
				fresh[change.Old] = change.New
			}
			for _, old := range []dto.UUID{nana, kai} {
				if fresh[old].IsNil() || fresh[old] == old {
					t.Fatalf("user %s not remapped: %+v", old, report.Users)
				}
				if counts := countUserReferences(t, stores, old); sumCounts(counts) != 0 {
					t.Errorf("old id %s still referenced: %v", old, counts)
				}
				after := countUserReferences(t, stores, fresh[old])
				for ref, want := range before[old] {
					if after[ref] != want {
						t.Errorf("%s rows for remapped %s = %d, want %d", ref, old, after[ref], want)
					}
				}
			}
			for _, ref := range store.UserIDReferences() {
				want := before[nana][ref.String()] + before[kai][ref.String()]
				if report.Rows[ref.String()] != int64(want) {
					t.Errorf("report rows %s = %d, want %d", ref, report.Rows[ref.String()], want)
				}
			}

			users, err := identity.ListUsers(ctx)
			if err != nil || len(users) != 2 {
				t.Fatalf("users after remap = %+v, %v", users, err)
			}
			moved, err := identity.UserByNormalizedName(ctx, "NANA")
			if err != nil {
				t.Fatal(err)
			}
			if moved.ID != fresh[nana] || moved.Name != "nana" || moved.PasswordHash == "" || string(moved.Policy) != `{"IsAdministrator":true}` || moved.LastLoginDate == nil || !moved.LastLoginDate.Equal(now) {
				t.Errorf("remapped user = %+v", moved)
			}
			for token, name := range map[string]string{"nana-token": "nana", "kai-token": "kai"} {
				device, err := identity.DeviceByToken(ctx, token)
				if err != nil {
					t.Fatalf("token %q no longer resolves: %v", token, err)
				}
				owner, err := identity.User(ctx, device.UserID)
				if err != nil || owner.Name != name {
					t.Errorf("token %q authenticates as %+v (%v), want %q", token, owner, err, name)
				}
			}
			state, err := items.Playlist(ctx, playlist.ID)
			if err != nil {
				t.Fatal(err)
			}
			if state.OwnerUserID != fresh[nana] || len(state.Shares) != 1 || state.Shares[0].UserID != fresh[kai] {
				t.Errorf("playlist owner/shares = %s %+v", state.OwnerUserID, state.Shares)
			}

			second, err := identity.RemapUserIDs(ctx, func() (dto.UUID, error) {
				t.Fatal("second run minted an id")
				return dto.UUID{}, nil
			}, now.Add(time.Hour), false)
			if err != nil {
				t.Fatalf("second run: %v", err)
			}
			if !second.AlreadyRemapped || second.RemappedAt != "2026-10-10T12:00:00Z" || len(second.Users) != 0 {
				t.Errorf("second report = %+v", second)
			}
			if again, err := identity.UserByNormalizedName(ctx, "NANA"); err != nil || again.ID != fresh[nana] {
				t.Errorf("second run changed ids: %+v, %v", again, err)
			}
		})
	}
}

// TestIdentityUserIDReferencesCoverSchema fails when a migration adds a
// column that names a user id but RemapUserIDs does not rewrite it.
func TestIdentityUserIDReferencesCoverSchema(t *testing.T) {
	stores := openRemapSQLite(t)
	known := map[string]bool{"users.id": true}
	for _, ref := range store.UserIDReferences() {
		known[ref.String()] = true
	}
	rows, err := stores.Reader().QueryContext(t.Context(), "SELECT m.name, p.name FROM sqlite_master AS m JOIN pragma_table_info(m.name) AS p WHERE m.type = 'table'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(column, "user_id") && !known[table+"."+column] {
			t.Errorf("%s.%s holds a user id but store.UserIDReferences lacks it", table, column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func countUserReferences(t *testing.T, stores store.Stores, id dto.UUID) map[string]int {
	t.Helper()
	var arg any = id[:]
	if stores.Dialect().Placeholder(1) == "$1" {
		arg = id.Dashed()
	}
	counts := map[string]int{}
	for _, ref := range store.UserIDReferences() {
		var count int
		query := "SELECT COUNT(*) FROM " + ref.Table + " WHERE " + ref.Column + " = " + stores.Dialect().Placeholder(1)
		if err := stores.Reader().QueryRowContext(t.Context(), query, arg).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", ref, err)
		}
		counts[ref.String()] = count
	}
	return counts
}

func sumCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}
