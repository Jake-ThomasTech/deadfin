// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	storetestpkg "github.com/deadfin-org/deadfin/internal/store/storetest"
)

// TestItemStoreMusicAlbumArtistsSQLite proves the album artist read and
// write against SQLite.
func TestItemStoreMusicAlbumArtistsSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runItemStoreMusicAlbumArtists(t, stores)
}

// TestItemStoreMusicAlbumArtistsPostgres runs the same cases against
// PostgreSQL when DEADFIN_TEST_POSTGRES_DSN is configured.
func TestItemStoreMusicAlbumArtistsPostgres(t *testing.T) {
	runItemStoreMusicAlbumArtists(t, storetestpkg.OpenPostgres(t))
}

// runItemStoreMusicAlbumArtists reads the artist values of each album and of
// its tracks (MusicAlbum.Tracks, disc folders included), then replaces the
// album values only and repairs the album user data key, which names the
// first album artist (MusicAlbum.GetUserDataKeys).
func runItemStoreMusicAlbumArtists(t *testing.T, stores store.Stores) {
	t.Helper()
	items := store.NewItemStore(stores)
	id := func(value int) dto.UUID {
		t.Helper()
		parsed, err := dto.ParseUUID(fmt.Sprintf("%032x", value))
		if err != nil {
			t.Fatalf("parse ID %d: %v", value, err)
		}
		return parsed
	}
	named := func(value int, kind item.Kind, parent dto.UUID, name string) item.Item {
		path := "/music/" + name
		return item.Item{ID: id(value), Kind: kind, ParentID: &parent, Name: &name, SortName: &name, Path: &path, IsFolder: kind != item.KindAudio}
	}
	library := item.Item{ID: id(1), Kind: item.KindCollectionFolder, IsFolder: true}
	artist := named(2, item.KindMusicArtist, library.ID, "Artist")
	album := named(10, item.KindMusicAlbum, artist.ID, "Album")
	album.Values.Genres = []string{"Rock"}
	disc := named(11, item.KindFolder, album.ID, "Album/CD 01")
	first := named(21, item.KindAudio, disc.ID, "Album/CD 01/01")
	first.Values = item.ItemValues{Artists: []string{"Artist", "Guest"}, AlbumArtists: []string{"Artist"}}
	second := named(22, item.KindAudio, album.ID, "Album/02")
	second.Values = item.ItemValues{Artists: []string{"Artist"}, AlbumArtists: []string{"Artist"}}
	untagged := named(23, item.KindAudio, album.ID, "Album/03")
	empty := named(30, item.KindMusicAlbum, artist.ID, "Empty")
	empty.IsLocked = true
	for _, value := range []item.Item{library, artist, album, disc, first, second, untagged, empty} {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
	}

	got, err := items.QueryMusicAlbumTracks(t.Context())
	if err != nil {
		t.Fatalf("query album tracks: %v", err)
	}
	want := []store.MusicAlbumTracks{
		{
			ID: album.ID,
			Tracks: []store.MusicAlbumArtists{
				{Artists: []string{"Artist"}, AlbumArtists: []string{"Artist"}},
				{},
				{Artists: []string{"Artist", "Guest"}, AlbumArtists: []string{"Artist"}},
			},
			Folders: []dto.UUID{album.ID, disc.ID},
		},
		{ID: empty.ID, IsLocked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("album tracks = %+v, want %+v", got, want)
	}

	update := store.MusicAlbumArtists{Artists: []string{"Artist", "Guest"}, AlbumArtists: []string{"Artist"}}
	if err := items.UpdateMusicAlbumArtists(t.Context(), []store.MusicAlbumArtistUpdate{{ID: album.ID, Values: update}}); err != nil {
		t.Fatalf("update album artists: %v", err)
	}
	stored, err := items.Get(t.Context(), album.ID)
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if !reflect.DeepEqual(stored.Values.Artists, update.Artists) || !reflect.DeepEqual(stored.Values.AlbumArtists, update.AlbumArtists) || !reflect.DeepEqual(stored.Values.Genres, []string{"Rock"}) {
		t.Fatalf("stored album values = %+v, want the update and the genre", stored.Values)
	}
	if !hasUserDataKey(t, stores, "Artist-Album") {
		t.Fatal("album user data keys lack Artist-Album")
	}

	got, err = items.QueryMusicAlbumTracks(t.Context())
	if err != nil {
		t.Fatalf("query album tracks after update: %v", err)
	}
	if !reflect.DeepEqual(got[0].Album, update) {
		t.Fatalf("album values after update = %+v, want %+v", got[0].Album, update)
	}

	if err := items.UpdateMusicAlbumArtists(t.Context(), []store.MusicAlbumArtistUpdate{{ID: album.ID}}); err != nil {
		t.Fatalf("clear album artists: %v", err)
	}
	stored, err = items.Get(t.Context(), album.ID)
	if err != nil {
		t.Fatalf("get cleared album: %v", err)
	}
	if len(stored.Values.Artists) != 0 || len(stored.Values.AlbumArtists) != 0 {
		t.Fatalf("cleared album values = %+v, want no artists", stored.Values)
	}
}

func hasUserDataKey(t *testing.T, stores store.Stores, key string) bool {
	t.Helper()
	rows, err := stores.Reader().QueryContext(t.Context(), "SELECT custom_key FROM item_user_data_keys")
	if err != nil {
		t.Fatalf("read user data keys: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan user data key: %v", err)
		}
		if value == key {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read user data keys: %v", err)
	}
	return false
}
