// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

// albumTagProbe returns the ARTIST and album_artist tags of each track by
// file name.
type albumTagProbe struct {
	mu   sync.Mutex
	tags map[string]library.MediaProbeResult
}

func (p *albumTagProbe) Probe(_ context.Context, input library.MediaProbeInput) (library.MediaProbeResult, error) {
	if input.Item.Kind != item.KindAudio {
		return library.MediaProbeResult{}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tags[filepath.Base(*input.Item.Path)], nil
}

func (p *albumTagProbe) set(name string, artists, albumArtists []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tags[name] = library.MediaProbeResult{Artists: artists, AlbumArtists: albumArtists}
}

// TestScanMergesAlbumArtistsFromSongs follows AlbumMetadataService: a
// MusicAlbum takes Artists and AlbumArtists from its songs, grouped without
// case and ordered by song count, and its DTO names them (#739).
func TestScanMergesAlbumArtistsFromSongs(t *testing.T) {
	stores := storetest.OpenCountingSQLite(t, "album-artists.db").Stores()
	items := store.NewItemStore(stores)
	music := filepath.Join(t.TempDir(), "music")
	probe := &albumTagProbe{tags: map[string]library.MediaProbeResult{}}
	// 1989: one album artist; a featured artist on one song.
	for _, name := range []string{"01 Welcome.mp3", "02 Blank Space.mp3", "03 Style.mp3"} {
		writeScannerFile(t, filepath.Join(music, "Taylor Swift", "1989 (Deluxe)", name), "audio")
		probe.set(name, []string{"Taylor Swift"}, []string{"Taylor Swift"})
	}
	probe.set("03 Style.mp3", []string{"Taylor Swift", "Kendrick Lamar"}, []string{"Taylor Swift"})
	// A multi-disc compilation: the album artist of most songs comes first,
	// and the first spelling of a name wins.
	compilation := filepath.Join(music, "Various", "Hits")
	tracks := []struct {
		path         string
		artists      []string
		albumArtists []string
	}{
		{filepath.Join("CD1", "c01.mp3"), []string{"Beta"}, []string{"Beta"}},
		{filepath.Join("CD1", "c02.mp3"), []string{"Alpha"}, []string{"Alpha"}},
		{filepath.Join("CD2", "c03.mp3"), []string{"alpha"}, []string{"alpha"}},
		{filepath.Join("CD2", "c04.mp3"), []string{"Gamma"}, []string{"Gamma"}},
	}
	for _, track := range tracks {
		writeScannerFile(t, filepath.Join(compilation, track.path), "audio")
		probe.set(filepath.Base(track.path), track.artists, track.albumArtists)
	}

	scanner := albumArtistScanner(t, stores, items, music, probe)
	scannerScan(t, scanner, "scan tagged albums")

	deluxe := albumNamed(t, items, "1989 (Deluxe)")
	if want := []string{"Taylor Swift"}; !reflect.DeepEqual(deluxe.Values.AlbumArtists, want) {
		t.Fatalf("1989 AlbumArtists = %q, want %q", deluxe.Values.AlbumArtists, want)
	}
	if want := []string{"Taylor Swift", "Kendrick Lamar"}; !reflect.DeepEqual(deluxe.Values.Artists, want) {
		t.Fatalf("1989 Artists = %q, want %q", deluxe.Values.Artists, want)
	}
	hits := albumNamed(t, items, "Hits")
	if want := []string{"Alpha", "Beta", "Gamma"}; !reflect.DeepEqual(hits.Values.AlbumArtists, want) {
		t.Fatalf("Hits AlbumArtists = %q, want %q", hits.Values.AlbumArtists, want)
	}
	if want := []string{"Alpha", "Beta", "Gamma"}; !reflect.DeepEqual(hits.Values.Artists, want) {
		t.Fatalf("Hits Artists = %q, want %q", hits.Values.Artists, want)
	}

	// The album DTO names the artists; a name with a MusicArtist item gets
	// its id (DtoService IHasArtist, IHasAlbumArtist).
	artist := musicArtistNamed(t, items, "Taylor Swift")
	routes := router.New()
	handlers.RegisterItems(routes, items, dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, nil, nil)), clock.NewReal())
	request := httptest.NewRequest(http.MethodGet, "/Items?IncludeItemTypes=MusicAlbum&Recursive=true&SortBy=SortName&EnableImages=false&EnableUserData=false", nil)
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("/Items albums status = %d: %s", response.Code, response.Body.String())
	}
	var page struct {
		Items []struct {
			Name         string
			AlbumArtist  string
			AlbumArtists []dto.NameGuidPair
			Artists      []string
			ArtistItems  []dto.NameGuidPair
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode /Items albums: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Name != "1989 (Deluxe)" {
		t.Fatalf("/Items albums = %+v, want 1989 (Deluxe) and Hits", page.Items)
	}
	got := page.Items[0]
	if got.AlbumArtist != "Taylor Swift" || !reflect.DeepEqual(got.AlbumArtists, []dto.NameGuidPair{{Name: "Taylor Swift", ID: artist.ID}}) {
		t.Fatalf("1989 DTO album artists = %q %+v, want Taylor Swift %s", got.AlbumArtist, got.AlbumArtists, artist.ID)
	}
	// The tag-artist pass gives Kendrick Lamar, named only in a song tag, a
	// MusicArtist item too, so both names resolve to items.
	if !reflect.DeepEqual(got.Artists, []string{"Taylor Swift", "Kendrick Lamar"}) || len(got.ArtistItems) != 2 ||
		!reflect.DeepEqual(got.ArtistItems[0], dto.NameGuidPair{Name: "Taylor Swift", ID: artist.ID}) ||
		got.ArtistItems[1].Name != "Kendrick Lamar" || reflect.ValueOf(got.ArtistItems[1].ID).IsZero() {
		t.Fatalf("1989 DTO artists = %q %+v, want both names and an item for each", got.Artists, got.ArtistItems)
	}
	if page.Items[1].AlbumArtist != "Alpha" {
		t.Fatalf("Hits DTO AlbumArtist = %q, want Alpha", page.Items[1].AlbumArtist)
	}
}

// TestScanKeepsAlbumArtistsWithoutChildChanges follows the refresh condition
// of AlbumMetadataService.UpdateMetadataFromChildren: an unchanged rescan
// keeps edited album artists, a changed song refreshes them, and a locked
// album keeps its values.
func TestScanKeepsAlbumArtistsWithoutChildChanges(t *testing.T) {
	stores := storetest.OpenCountingSQLite(t, "album-artists-refresh.db").Stores()
	items := store.NewItemStore(stores)
	music := filepath.Join(t.TempDir(), "music")
	probe := &albumTagProbe{tags: map[string]library.MediaProbeResult{}}
	writeScannerFile(t, filepath.Join(music, "Band", "Record", "01.mp3"), "audio")
	probe.set("01.mp3", []string{"Band"}, []string{"Band"})
	scanner := albumArtistScanner(t, stores, items, music, probe)
	scannerScan(t, scanner, "first scan")
	album := albumNamed(t, items, "Record")
	if want := []string{"Band"}; !reflect.DeepEqual(album.Values.AlbumArtists, want) {
		t.Fatalf("first scan AlbumArtists = %q, want %q", album.Values.AlbumArtists, want)
	}

	edited := store.MusicAlbumArtists{Artists: []string{"Edited"}, AlbumArtists: []string{"Edited"}}
	if err := items.UpdateMusicAlbumArtists(t.Context(), []store.MusicAlbumArtistUpdate{{ID: album.ID, Values: edited}}); err != nil {
		t.Fatalf("edit album artists: %v", err)
	}
	scannerScan(t, scanner, "unchanged rescan")
	if got := albumNamed(t, items, "Record").Values.AlbumArtists; !reflect.DeepEqual(got, edited.AlbumArtists) {
		t.Fatalf("unchanged rescan AlbumArtists = %q, want the edit %q", got, edited.AlbumArtists)
	}

	writeScannerFile(t, filepath.Join(music, "Band", "Record", "02.mp3"), "audio")
	probe.set("02.mp3", []string{"Band"}, []string{"Band"})
	scannerScan(t, scanner, "rescan with a new song")
	album = albumNamed(t, items, "Record")
	if want := []string{"Band"}; !reflect.DeepEqual(album.Values.AlbumArtists, want) || !reflect.DeepEqual(album.Values.Artists, want) {
		t.Fatalf("changed rescan artists = %q / %q, want %q", album.Values.Artists, album.Values.AlbumArtists, want)
	}

	album.IsLocked = true
	album.Values.AlbumArtists = []string{"Locked"}
	if err := items.Upsert(t.Context(), album); err != nil {
		t.Fatalf("lock album: %v", err)
	}
	writeScannerFile(t, filepath.Join(music, "Band", "Record", "03.mp3"), "audio")
	probe.set("03.mp3", []string{"Other"}, []string{"Other"})
	scannerScan(t, scanner, "rescan of a locked album")
	if got := albumNamed(t, items, "Record").Values.AlbumArtists; !reflect.DeepEqual(got, []string{"Locked"}) {
		t.Fatalf("locked album AlbumArtists = %q, want [Locked]", got)
	}
}

func albumArtistScanner(t *testing.T, stores store.Stores, items *store.ItemStore, music string, probe library.MediaProbe) *library.Scanner {
	t.Helper()
	topology, err := library.NewTopology(t.TempDir(), store.NewLibraryStore(stores))
	if err != nil {
		t.Fatalf("create topology: %v", err)
	}
	t.Cleanup(func() {
		if err := topology.Close(); err != nil {
			t.Errorf("close topology: %v", err)
		}
	})
	if _, err := topology.AddVirtualFolder(t.Context(), "Music", "music", library.LibraryOptions{PathInfos: []library.MediaPathInfo{{Path: music}}}); err != nil {
		t.Fatalf("add music library: %v", err)
	}
	scanner, err := library.NewScanner(topology, items, library.NewDefaultChain(naming.DefaultOptions()), library.ScannerOptions{Concurrency: 1, MediaProbe: probe})
	if err != nil {
		t.Fatalf("create scanner: %v", err)
	}
	t.Cleanup(func() {
		if err := scanner.Close(); err != nil {
			t.Errorf("close scanner: %v", err)
		}
	})
	return scanner
}

func albumNamed(t *testing.T, items *store.ItemStore, name string) item.Item {
	t.Helper()
	return itemNamed(t, items, item.KindMusicAlbum, name)
}

func musicArtistNamed(t *testing.T, items *store.ItemStore, name string) item.Item {
	t.Helper()
	return itemNamed(t, items, item.KindMusicArtist, name)
}

func itemNamed(t *testing.T, items *store.ItemStore, kind item.Kind, name string) item.Item {
	t.Helper()
	page, err := items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{kind}, Recursive: true})
	if err != nil {
		t.Fatalf("query %s items: %v", kind, err)
	}
	for _, value := range page.Items {
		if value.Name != nil && *value.Name == name {
			return value
		}
	}
	t.Fatalf("no %s named %q in %d items", kind, name, len(page.Items))
	return item.Item{}
}
