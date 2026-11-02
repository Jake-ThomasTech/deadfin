// SPDX-License-Identifier: GPL-2.0-only

package jellyfinsource_test

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

func TestSourceReadsTrickplayInfosWithItemPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jellyfin.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE BaseItems (Id TEXT PRIMARY KEY, Type TEXT, Name TEXT, Path TEXT)`,
		`CREATE TABLE TrickplayInfos (ItemId TEXT, Width INTEGER, Bandwidth INTEGER, Height INTEGER, Interval INTEGER, ThumbnailCount INTEGER, TileHeight INTEGER, TileWidth INTEGER, PRIMARY KEY (ItemId, Width))`,
		`INSERT INTO BaseItems VALUES ('AAAAAAAA-0000-0000-0000-000000000001','MediaBrowser.Controller.Entities.Movies.Movie','Heat','/media/movies/Heat (1995)/Heat.mkv')`,
		`INSERT INTO TrickplayInfos VALUES ('AAAAAAAA-0000-0000-0000-000000000001',320,26560,180,10000,570,10,10),
			('BBBBBBBB-0000-0000-0000-000000000001',320,1,1,10000,1,10,10)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture: %v\n%s", err, statement)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	src, err := jellyfinsource.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	got, err := src.TrickplayInfos(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	itemID, _ := dto.ParseUUID("aaaaaaaa000000000000000000000001")
	want := []jellyfinsource.TrickplayInfo{{
		ItemID: itemID, ItemPath: "/media/movies/Heat (1995)/Heat.mkv", Width: 320, Height: 180,
		TileWidth: 10, TileHeight: 10, ThumbnailCount: 570, Interval: 10000, Bandwidth: 26560,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TrickplayInfos = %+v, want only the row whose item exists %+v", got, want)
	}
}

func TestSourceWithoutTrickplayInfosTableHasNone(t *testing.T) {
	src, err := jellyfinsource.Open(t.Context(), fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	got, err := src.TrickplayInfos(t.Context())
	if err != nil || len(got) != 0 {
		t.Fatalf("TrickplayInfos = %+v, %v; want none", got, err)
	}
}
