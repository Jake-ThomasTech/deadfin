// SPDX-License-Identifier: GPL-2.0-only

package storetest_test

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
	storetestpkg "github.com/deadfin-org/deadfin/internal/store/storetest"
)

// TestItemStoreArtistsByNameSQLite proves the ArtistsValidator store reads
// and writes against SQLite.
func TestItemStoreArtistsByNameSQLite(t *testing.T) {
	stores, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	runItemStoreArtistsByName(t, stores)
}

// TestItemStoreArtistsByNamePostgres runs the same cases against PostgreSQL
// when DEADFIN_TEST_POSTGRES_DSN is configured.
func TestItemStoreArtistsByNamePostgres(t *testing.T) {
	runItemStoreArtistsByName(t, storetestpkg.OpenPostgres(t))
}

// runItemStoreArtistsByName reads the artist names of all items, inserts
// only the missing artists, and finds the unlocked artists accessed by name
// that no item names (BaseItemRepository.GetAllArtistNames,
// InternalItemsQuery.IsDeadArtist).
func runItemStoreArtistsByName(t *testing.T, stores store.Stores) {
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
	named := func(value int, kind item.Kind, parent *dto.UUID, name string) item.Item {
		return item.Item{ID: id(value), Kind: kind, ParentID: parent, Name: &name, SortName: &name, IsFolder: kind != item.KindAudio}
	}
	library := item.Item{ID: id(1), Kind: item.KindCollectionFolder, IsFolder: true}
	folderArtist := named(2, item.KindMusicArtist, &library.ID, "Folder Artist")
	album := named(3, item.KindMusicAlbum, &folderArtist.ID, "Album")
	track := named(4, item.KindAudio, &album.ID, "Track")
	track.Values = item.ItemValues{Artists: []string{"Folder Artist", "Guest"}, AlbumArtists: []string{"Folder Artist"}, Genres: []string{"Rock"}}
	other := named(5, item.KindAudio, &album.ID, "Other")
	other.Values = item.ItemValues{Artists: []string{"guest"}, AlbumArtists: []string{"Compiler"}}
	for _, value := range []item.Item{library, folderArtist, album, track, other} {
		if err := items.Upsert(t.Context(), value); err != nil {
			t.Fatalf("upsert %s: %v", value.ID, err)
		}
	}

	names, err := items.ArtistNames(t.Context())
	if err != nil {
		t.Fatalf("artist names: %v", err)
	}
	sort.Strings(names)
	if want := []string{"Compiler", "Folder Artist", "Guest", "guest"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("artist names = %q, want %q", names, want)
	}

	guest := named(10, item.KindMusicArtist, nil, "Guest")
	compiler := named(11, item.KindMusicArtist, nil, "Compiler")
	inserted, err := items.InsertMissingItems(t.Context(), []item.Item{guest, compiler})
	if err != nil || inserted != 2 {
		t.Fatalf("insert artists = %d, %v; want 2", inserted, err)
	}
	renamed := guest
	renamed.Name = stringPointer("Renamed")
	inserted, err = items.InsertMissingItems(t.Context(), []item.Item{renamed})
	if err != nil || inserted != 0 {
		t.Fatalf("insert stored artist = %d, %v; want 0", inserted, err)
	}
	stored, err := items.Get(t.Context(), guest.ID)
	if err != nil || stored.Name == nil || *stored.Name != "Guest" {
		t.Fatalf("stored artist = %+v, %v; want Guest kept", stored.Name, err)
	}

	dead, err := items.DeadArtistIDs(t.Context())
	if err != nil || len(dead) != 0 {
		t.Fatalf("dead artists = %v, %v; want none", dead, err)
	}
	other.Values = item.ItemValues{Artists: []string{"Folder Artist"}}
	if err := items.Upsert(t.Context(), other); err != nil {
		t.Fatalf("retag other: %v", err)
	}
	lockedName := "Locked"
	locked := named(12, item.KindMusicArtist, nil, lockedName)
	locked.IsLocked = true
	if _, err := items.InsertMissingItems(t.Context(), []item.Item{locked}); err != nil {
		t.Fatalf("insert locked artist: %v", err)
	}
	shadowed := named(13, item.KindMusicArtist, nil, "folder artist")
	if _, err := items.InsertMissingItems(t.Context(), []item.Item{shadowed}); err != nil {
		t.Fatalf("insert shadowed artist: %v", err)
	}
	dead, err = items.DeadArtistIDs(t.Context())
	if err != nil {
		t.Fatalf("dead artists after retag: %v", err)
	}
	if want := []dto.UUID{compiler.ID, shadowed.ID}; !reflect.DeepEqual(dead, want) {
		t.Fatalf("dead artists after retag = %v, want %v (Guest still named, folder artist and locked artist kept, by-name twin of the folder artist dead)", dead, want)
	}
}

func stringPointer(value string) *string { return &value }
