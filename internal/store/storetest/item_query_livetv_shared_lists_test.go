// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

func TestItemStoreQueryLiveTvSharedListsSQLite(t *testing.T) {
	runItemStoreQueryLiveTvSharedLists(t, openItemStore)
}

func TestItemStoreQueryLiveTvSharedListsPostgres(t *testing.T) {
	runItemStoreQueryLiveTvSharedLists(t, openPostgresItemStore)
}

// runItemStoreQueryLiveTvSharedLists checks LimitLiveTvToSharedLists (D-105):
// a channel is visible only through a public, owned or shared playlist, a
// programme follows its channel, and other kinds do not change.
func runItemStoreQueryLiveTvSharedLists(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()
	ctx := t.Context()
	items := open(t)
	folder := groupingAccessItem(t, 901, item.KindUserView, "Live TV")
	folder.IsFolder, folder.SourceType = true, item.SourceTypeLiveTV
	groupingAccessUpsert(t, items, folder)
	movie := groupingAccessItem(t, 902, item.KindMovie, "movie")
	groupingAccessUpsert(t, items, movie)
	channels := make([]item.Item, 0, 4)
	for index, name := range []string{"public", "owned", "shared", "private"} {
		channel := groupingAccessItem(t, 910+index, item.KindLiveTvChannel, "channel "+name)
		channel.ParentID, channel.SourceType = &folder.ID, item.SourceTypeLiveTV
		groupingAccessUpsert(t, items, channel)
		channels = append(channels, channel)
	}
	programs := make([]item.Item, 0, 2)
	for index, channel := range []item.Item{channels[2], channels[3]} {
		program := groupingAccessItem(t, 920+index, item.KindLiveTvProgram, "programme "+*channel.Name)
		program.ChannelID, program.SourceType = &channel.ID, item.SourceTypeLiveTV
		groupingAccessUpsert(t, items, program)
		programs = append(programs, program)
	}

	viewer := groupingAccessUUID(t, 930)
	other := groupingAccessUUID(t, 931)
	lists := []struct {
		value  int
		owner  dto.UUID
		public bool
		shares []store.ItemShare
		child  item.Item
	}{
		{941, other, true, nil, channels[0]},
		{942, viewer, false, nil, channels[1]},
		{943, other, false, []store.ItemShare{{UserID: viewer}}, channels[2]},
		{944, other, false, nil, channels[3]},
	}
	for _, list := range lists {
		playlist := groupingAccessItem(t, list.value, item.KindPlaylist, "list "+*list.child.Name)
		playlist.IsFolder, playlist.PlaylistOwnerUserID, playlist.OpenAccess = true, list.owner, list.public
		if err := items.CreatePlaylist(ctx, playlist, list.shares); err != nil {
			t.Fatal(err)
		}
		if _, err := items.MutatePlaylist(ctx, playlist.ID, func(state *store.PlaylistState) error {
			state.Children = []dto.UUID{list.child.ID}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	query := func(policy string) store.ItemQuery {
		return store.ItemQuery{
			User:             &store.User{ID: viewer, Policy: []byte(policy)},
			Recursive:        true,
			IncludeItemTypes: []item.Kind{item.KindMovie, item.KindLiveTvChannel, item.KindLiveTvProgram},
		}
	}
	all := []dto.UUID{channels[1].ID, channels[3].ID, channels[0].ID, channels[2].ID, movie.ID, programs[1].ID, programs[0].ID}
	groupingAccessAssertQuery(t, items, query(`{"EnableAllFolders":true}`), all, 7)
	groupingAccessAssertQuery(t, items, query(`{"EnableAllFolders":true,"LimitLiveTvToSharedLists":false}`), all, 7)
	groupingAccessAssertQuery(t, items, query(`{"IsAdministrator":true,"EnableAllFolders":true,"LimitLiveTvToSharedLists":true}`), all, 7)
	groupingAccessAssertQuery(t, items, query(`{"EnableAllFolders":true,"LimitLiveTvToSharedLists":true}`),
		[]dto.UUID{channels[1].ID, channels[0].ID, channels[2].ID, movie.ID, programs[0].ID}, 5)
}
