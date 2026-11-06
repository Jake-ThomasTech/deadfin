// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

func TestItemStoreQueryTvChannelKindSQLite(t *testing.T) {
	runItemStoreQueryTvChannelKind(t, openItemStore)
}

func TestItemStoreQueryTvChannelKindPostgres(t *testing.T) {
	runItemStoreQueryTvChannelKind(t, openPostgresItemStore)
}

// runItemStoreQueryTvChannelKind checks ItemTypeLookup: the client kinds
// TvChannel and TvProgram include and exclude the stored LiveTvChannel and
// LiveTvProgram rows.
func runItemStoreQueryTvChannelKind(t *testing.T, open func(*testing.T) *store.ItemStore) {
	t.Helper()
	items := open(t)
	folder := groupingAccessItem(t, 801, item.KindUserView, "Live TV")
	folder.IsFolder, folder.SourceType = true, item.SourceTypeLiveTV
	channel := groupingAccessItem(t, 802, item.KindLiveTvChannel, "channel")
	channel.ParentID, channel.SourceType = &folder.ID, item.SourceTypeLiveTV
	program := groupingAccessItem(t, 803, item.KindLiveTvProgram, "program")
	program.ParentID, program.SourceType = &channel.ID, item.SourceTypeLiveTV
	movie := groupingAccessItem(t, 804, item.KindMovie, "movie")
	for _, value := range []item.Item{folder, channel, program, movie} {
		groupingAccessUpsert(t, items, value)
	}
	user := &store.User{ID: groupingAccessUUID(t, 805), Policy: []byte(`{"EnableAllFolders":true,"EnableLiveTvAccess":true}`)}
	include := func(kinds ...item.Kind) store.ItemQuery {
		return store.ItemQuery{User: user, Recursive: true, IncludeItemTypes: kinds}
	}
	groupingAccessAssertQuery(t, items, include(item.KindTvChannel), []dto.UUID{channel.ID}, 1)
	groupingAccessAssertQuery(t, items, include(item.KindTvProgram), []dto.UUID{program.ID}, 1)
	groupingAccessAssertQuery(t, items, include(item.KindTvChannel, item.KindLiveTvChannel), []dto.UUID{channel.ID}, 1)
	exclude := store.ItemQuery{User: user, Recursive: true, ExcludeItemTypes: []item.Kind{item.KindTvChannel, item.KindTvProgram, item.KindUserView}}
	groupingAccessAssertQuery(t, items, exclude, []dto.UUID{movie.ID}, 1)
}
