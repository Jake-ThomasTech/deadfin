// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

func TestItemStoreQueryLiveTvAccessSQLite(t *testing.T) {
	runItemStoreQueryLiveTvAccess(t, openItemStore)
}

func TestItemStoreQueryLiveTvAccessPostgres(t *testing.T) {
	runItemStoreQueryLiveTvAccess(t, openPostgresItemStore)
}

// runItemStoreQueryLiveTvAccess checks that EnabledFolders does not hide Live
// TV items and that EnableLiveTvAccess does.
func runItemStoreQueryLiveTvAccess(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()
	items := open(t)
	library := groupingAccessItem(t, 701, item.KindFolder, "library")
	library.IsFolder = true
	hidden := groupingAccessItem(t, 702, item.KindFolder, "hidden")
	hidden.IsFolder = true
	movie := groupingAccessItem(t, 703, item.KindMovie, "hidden movie")
	movie.ParentID = &hidden.ID
	folder := groupingAccessItem(t, 704, item.KindUserView, "Live TV")
	folder.IsFolder, folder.SourceType = true, item.SourceTypeLiveTV
	channel := groupingAccessItem(t, 705, item.KindLiveTvChannel, "channel")
	channel.ParentID, channel.SourceType = &folder.ID, item.SourceTypeLiveTV
	for _, value := range []item.Item{library, hidden, movie, folder, channel} {
		groupingAccessUpsert(t, items, value)
	}
	query := func(policy string) store.ItemQuery {
		return store.ItemQuery{
			User:             &store.User{ID: groupingAccessUUID(t, 706), Policy: []byte(policy)},
			Recursive:        true,
			IncludeItemTypes: []item.Kind{item.KindMovie, item.KindLiveTvChannel},
		}
	}
	enabled := `{"EnableAllFolders":false,"EnabledFolders":["` + library.ID.String() + `"]`
	groupingAccessAssertQuery(t, items, query(enabled+`}`), []dto.UUID{channel.ID}, 1)
	groupingAccessAssertQuery(t, items, query(enabled+`,"EnableLiveTvAccess":true}`), []dto.UUID{channel.ID}, 1)
	groupingAccessAssertQuery(t, items, query(enabled+`,"EnableLiveTvAccess":false}`), []dto.UUID{}, 0)
	groupingAccessAssertQuery(t, items, query(`{"EnableAllFolders":true}`), []dto.UUID{channel.ID, movie.ID}, 2)
	groupingAccessAssertQuery(t, items, query(`{"EnableAllFolders":true,"EnableLiveTvAccess":false}`), []dto.UUID{movie.ID}, 1)
}

func TestItemStoreChannelListsSQLite(t *testing.T) {
	runItemStoreChannelLists(t, openItemStore)
}

func TestItemStoreChannelListsPostgres(t *testing.T) {
	runItemStoreChannelLists(t, openPostgresItemStore)
}

// runItemStoreChannelLists checks a playlist and a collection of Live TV
// channels (#719): the playlist keeps its entry order, both list the
// channels as their children, and a user without Live TV access sees none
// of them (D-95).
func runItemStoreChannelLists(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()
	ctx := t.Context()
	items := open(t)
	folder := groupingAccessItem(t, 801, item.KindUserView, "Live TV")
	folder.IsFolder, folder.SourceType = true, item.SourceTypeLiveTV
	groupingAccessUpsert(t, items, folder)
	channels := make([]dto.UUID, 0, 5)
	for index, name := range []string{"a", "b", "c", "d", "e"} {
		channel := groupingAccessItem(t, 810+index, item.KindLiveTvChannel, name)
		channel.ParentID, channel.SourceType = &folder.ID, item.SourceTypeLiveTV
		groupingAccessUpsert(t, items, channel)
		channels = append(channels, channel.ID)
	}
	owner := groupingAccessUUID(t, 820)
	playlist := groupingAccessItem(t, 821, item.KindPlaylist, "Mom")
	playlist.IsFolder, playlist.PlaylistOwnerUserID, playlist.OpenAccess = true, owner, true
	if err := items.CreatePlaylist(ctx, playlist, nil); err != nil {
		t.Fatal(err)
	}
	curated := []dto.UUID{channels[3], channels[0], channels[4], channels[1]}
	if _, err := items.MutatePlaylist(ctx, playlist.ID, func(state *store.PlaylistState) error {
		state.Children = curated
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	collection := groupingAccessItem(t, 822, item.KindBoxSet, "Cable")
	collection.IsFolder = true
	groupingAccessUpsert(t, items, collection)
	if err := items.AddToCollection(ctx, collection.ID, curated); err != nil {
		t.Fatal(err)
	}

	children := func(t *testing.T, parent dto.UUID, policy string) []dto.UUID {
		t.Helper()
		user := store.User{ID: owner, Policy: []byte(policy)}
		got, err := items.Query(ctx, store.ItemQuery{ParentID: &parent, User: &user, UserID: &user.ID})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]dto.UUID, len(got.Items))
		for index, value := range got.Items {
			if value.Kind != item.KindLiveTvChannel {
				t.Fatalf("child %s kind %s", value.ID, value.Kind)
			}
			ids[index] = value.ID
		}
		return ids
	}
	for _, policy := range []string{`{"EnableAllFolders":true}`, `{"EnableAllFolders":false,"EnabledFolders":[]}`} {
		if got := children(t, playlist.ID, policy); !reflect.DeepEqual(got, curated) {
			t.Fatalf("playlist children for %s = %v, want %v", policy, got, curated)
		}
		got := children(t, collection.ID, policy)
		slices.SortFunc(got, func(a, b dto.UUID) int { return strings.Compare(a.String(), b.String()) })
		want := slices.Clone(curated)
		slices.SortFunc(want, func(a, b dto.UUID) int { return strings.Compare(a.String(), b.String()) })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("collection children for %s = %v, want %v", policy, got, want)
		}
	}
	for _, policy := range []string{`{"EnableAllFolders":true,"EnableLiveTvAccess":false}`, `{"EnableAllFolders":false,"EnabledFolders":[],"EnableLiveTvAccess":false}`} {
		for _, parent := range []dto.UUID{playlist.ID, collection.ID} {
			if got := children(t, parent, policy); len(got) != 0 {
				t.Fatalf("children of %s for %s = %v, want none", parent, policy, got)
			}
		}
	}
}
