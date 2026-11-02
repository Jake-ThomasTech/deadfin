// SPDX-License-Identifier: GPL-2.0-only

package jellyfinsource_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

const (
	containerOwner  = "4E703B0E-6E78-4EA6-9C48-43A2654C5CB1"
	containerFriend = "5F703B0E-6E78-4EA6-9C48-43A2654C5CB2"
	containerList   = "CCCCCCCC-0000-0000-0000-000000000001"
)

// containerFixture builds a jellyfin.db with one playlist and three movies.
// With linkedTable, the entries live in the LinkedChildren table; otherwise
// they live in the playlist's Data JSON as Jellyfin 10.11 writes them.
func containerFixture(t *testing.T, linkedTable bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jellyfin.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	data := `{"OwnerUserId":"4e703b0e6e784ea69c4843a2654c5cb1","OpenAccess":true,` +
		`"Shares":[{"UserId":"5f703b0e6e784ea69c4843a2654c5cb2","CanEdit":true}],"PlaylistMediaType":"Video",` +
		`"LinkedChildren":[{"Path":"/media/b.mkv","ItemId":"aaaaaaaa000000000000000000000002","Type":0},` +
		`{"Path":"/media/stale-a.mkv","LibraryItemId":"aaaaaaaa000000000000000000000001","Type":"Manual"},` +
		`{"Path":"/media/shortcut.mkv","Type":1},` +
		`{"Path":"/media/c.mkv"},` +
		`{"ItemId":"aaaaaaaa0000000000000000000000ff"}]}`
	statements := []string{
		`CREATE TABLE BaseItems (Id TEXT PRIMARY KEY, Type TEXT, Name TEXT, Path TEXT, Data TEXT, DateCreated TEXT)`,
		`INSERT INTO BaseItems VALUES ('AAAAAAAA-0000-0000-0000-000000000001','MediaBrowser.Controller.Entities.Movies.Movie','A','/media/a.mkv',NULL,NULL),
			('AAAAAAAA-0000-0000-0000-000000000002','MediaBrowser.Controller.Entities.Movies.Movie','B','/media/b.mkv',NULL,NULL),
			('AAAAAAAA-0000-0000-0000-000000000003','MediaBrowser.Controller.Entities.Movies.Movie','C','/media/c.mkv',NULL,NULL),
			('` + containerList + `','MediaBrowser.Controller.Playlists.Playlist','Road trip','/config/data/playlists/Road trip','` + data + `','2025-03-04 05:06:07')`,
	}
	if linkedTable {
		statements = append(statements,
			`CREATE TABLE LinkedChildren (ParentId TEXT, ChildId TEXT, ChildType INTEGER, SortOrder INTEGER)`,
			`INSERT INTO LinkedChildren VALUES ('`+containerList+`','AAAAAAAA-0000-0000-0000-000000000003',0,2),
				('`+containerList+`','AAAAAAAA-0000-0000-0000-000000000001',0,0),
				('`+containerList+`','AAAAAAAA-0000-0000-0000-000000000002',1,1),
				('`+containerList+`','AAAAAAAA-0000-0000-0000-0000000000FF',0,3)`)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture: %v\n%s", err, statement)
		}
	}
	return path
}

func TestSourceReadsPlaylists(t *testing.T) {
	for _, test := range []struct {
		name        string
		linkedTable bool
		children    []string
		unresolved  int
	}{
		// ItemId wins, then LibraryItemId, then Path; Shortcut entries drop.
		{name: "data json", children: []string{"/media/b.mkv", "/media/a.mkv", "/media/c.mkv"}, unresolved: 1},
		// Manual rows in SortOrder; the missing child has no path.
		{name: "linked children table", linkedTable: true, children: []string{"/media/a.mkv", "/media/c.mkv"}, unresolved: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			src, err := jellyfinsource.Open(t.Context(), containerFixture(t, test.linkedTable))
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			values, err := src.Containers(t.Context(), jellyfinsource.PlaylistType)
			if err != nil {
				t.Fatal(err)
			}
			if len(values) != 1 {
				t.Fatalf("containers = %+v", values)
			}
			got := values[0]
			owner, _ := dto.ParseUUID(containerOwner)
			friend, _ := dto.ParseUUID(containerFriend)
			if got.Name != "Road trip" || got.OwnerUserID != owner || !got.OpenAccess || got.MediaType != "Video" {
				t.Fatalf("container = %+v", got)
			}
			if !reflect.DeepEqual(got.Shares, []jellyfinsource.Share{{UserID: friend, CanEdit: true}}) {
				t.Fatalf("shares = %+v", got.Shares)
			}
			if got.DateCreated == nil || got.DateCreated.Year() != 2025 {
				t.Fatalf("date created = %v", got.DateCreated)
			}
			if !reflect.DeepEqual(got.ChildPaths, test.children) || got.UnresolvedChildren != test.unresolved {
				t.Fatalf("children = %v (unresolved %d), want %v (unresolved %d)", got.ChildPaths, got.UnresolvedChildren, test.children, test.unresolved)
			}
		})
	}
}

// TestSourceReadsCollectionProvidersAndImages proves that a collection
// carries its BaseItemProviders rows and its BaseItemImageInfos rows, with
// %AppDataPath% and %MetadataPath% expanded beside the database
// (deadfin#808).
func TestSourceReadsCollectionProvidersAndImages(t *testing.T) {
	const boxSet = "DDDDDDDD-0000-0000-0000-000000000001"
	root := t.TempDir()
	path := filepath.Join(root, "data", "jellyfin.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE BaseItems (Id TEXT PRIMARY KEY, Type TEXT, Name TEXT, Path TEXT, Data TEXT, DateCreated TEXT)`,
		`INSERT INTO BaseItems VALUES ('` + boxSet + `','MediaBrowser.Controller.Entities.Movies.BoxSet','Cars Collection','%AppDataPath%/collections/Cars Collection [boxset]',NULL,NULL)`,
		`CREATE TABLE BaseItemProviders (ItemId TEXT, ProviderId TEXT, ProviderValue TEXT)`,
		`INSERT INTO BaseItemProviders VALUES ('` + boxSet + `','Tmdb','87118'), ('` + boxSet + `','Imdb','')`,
		`CREATE TABLE BaseItemImageInfos (Id TEXT, Path TEXT, DateModified TEXT, ImageType INTEGER, Width INTEGER, Height INTEGER, Blurhash BLOB, ItemId TEXT)`,
		`INSERT INTO BaseItemImageInfos VALUES ('E2','%MetadataPath%/library/dd/dddd/backdrop.jpg',NULL,2,0,0,NULL,'` + boxSet + `'),
			('E1','%AppDataPath%/collections/Cars Collection [boxset]/poster.jpg',NULL,0,0,0,NULL,'` + boxSet + `'),
			('E3','https://image.tmdb.org/t/p/original/cars.jpg',NULL,5,0,0,NULL,'` + boxSet + `')`,
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
	values, err := src.Containers(t.Context(), jellyfinsource.BoxSetType)
	if err != nil || len(values) != 1 {
		t.Fatalf("containers = %+v, %v", values, err)
	}
	if want := map[string]string{"Tmdb": "87118"}; !reflect.DeepEqual(values[0].ProviderIDs, want) {
		t.Errorf("provider ids = %v, want %v", values[0].ProviderIDs, want)
	}
	want := []jellyfinsource.Image{
		{Type: 0, Path: filepath.Join(root, "data", "collections", "Cars Collection [boxset]", "poster.jpg")},
		{Type: 2, Path: filepath.Join(root, "metadata", "library", "dd", "dddd", "backdrop.jpg")},
		{Type: 5, Path: "https://image.tmdb.org/t/p/original/cars.jpg"},
	}
	if !reflect.DeepEqual(values[0].Images, want) {
		t.Errorf("images = %#v, want %#v", values[0].Images, want)
	}
}
