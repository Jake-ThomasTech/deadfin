// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/storetest"
)

// tagArtistProbe returns the ARTIST and album_artist tags of each track by
// file name.
type tagArtistProbe struct {
	mu   sync.Mutex
	tags map[string]library.MediaProbeResult
}

func (p *tagArtistProbe) Probe(_ context.Context, input library.MediaProbeInput) (library.MediaProbeResult, error) {
	if input.Item.Kind != item.KindAudio {
		return library.MediaProbeResult{}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tags[filepath.Base(*input.Item.Path)], nil
}

// TestScanCreatesArtistsFromTags follows ArtistsValidator: every ARTIST and
// album_artist tag name gets a MusicArtist accessed by name, an artist
// folder serves its own name, names that differ in case share one artist,
// and an artist that no item names any more is deleted (#739).
func TestScanCreatesArtistsFromTags(t *testing.T) {
	stores := storetest.OpenCountingSQLite(t, "tag-artists.db").Stores()
	items := store.NewItemStore(stores)
	music := filepath.Join(t.TempDir(), "music")
	probe := &tagArtistProbe{tags: map[string]library.MediaProbeResult{
		"01.mp3": {Artists: []string{"Taylor Swift"}, AlbumArtists: []string{"Taylor Swift"}},
		"02.mp3": {Artists: []string{"Taylor Swift", "Kendrick Lamar"}, AlbumArtists: []string{"taylor swift"}},
		"03.mp3": {Artists: []string{"Guest: One"}, AlbumArtists: []string{"Compilers"}},
	}}
	writeScannerFile(t, filepath.Join(music, "Taylor Swift", "1989", "01.mp3"), "audio")
	writeScannerFile(t, filepath.Join(music, "Taylor Swift", "1989", "02.mp3"), "audio")
	writeScannerFile(t, filepath.Join(music, "Various", "Hits", "03.mp3"), "audio")

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

	scannerScan(t, scanner, "scan tagged tracks")
	scannerScan(t, scanner, "rescan tagged tracks")
	artists := tagArtists(t, items)
	if want := []string{"Compilers", "Guest: One", "Kendrick Lamar", "Taylor Swift", "Various"}; !reflect.DeepEqual(sortedNames(artists), want) {
		t.Fatalf("MusicArtist names = %q, want %q", sortedNames(artists), want)
	}
	for _, name := range []string{"Compilers", "Guest: One", "Kendrick Lamar"} {
		value := artists[name]
		want := library.ArtistEntity(name)
		if value.ID != want.ID || value.ParentID != nil || value.Path == nil || *value.Path != *want.Path {
			t.Fatalf("artist %q = id %s parent %v path %v, want accessed by name at %s %s", name, value.ID, value.ParentID, value.Path, want.ID, *want.Path)
		}
	}
	if want := "%MetadataPath%/artists/Guest  One"; *artists["Guest: One"].Path != want {
		t.Fatalf("artist path = %q, want %q", *artists["Guest: One"].Path, want)
	}
	if artists["Taylor Swift"].ParentID == nil {
		t.Fatal("Taylor Swift is not the artist folder")
	}

	routes := router.New()
	handlers.RegisterItemNames(routes, items, dtobuild.NewBuilder(items, dtobuild.NewStoreEnricher(items, nil, nil)), clock.NewReal())
	if got, want := artistEndpointNames(t, routes, "/Artists"), []string{"Compilers", "Guest: One", "Kendrick Lamar", "Taylor Swift"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("/Artists = %q, want %q", got, want)
	}
	if got, want := artistEndpointNames(t, routes, "/Artists/AlbumArtists"), []string{"Compilers", "Taylor Swift"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("/Artists/AlbumArtists = %q, want %q", got, want)
	}

	if err := os.Remove(filepath.Join(music, "Taylor Swift", "1989", "02.mp3")); err != nil {
		t.Fatalf("remove featured track: %v", err)
	}
	scannerScan(t, scanner, "rescan without the featured track")
	if want := []string{"Compilers", "Guest: One", "Taylor Swift", "Various"}; !reflect.DeepEqual(sortedNames(tagArtists(t, items)), want) {
		t.Fatalf("MusicArtist names after removal = %q, want %q", sortedNames(tagArtists(t, items)), want)
	}
}

func tagArtists(t *testing.T, items *store.ItemStore) map[string]item.Item {
	t.Helper()
	page, err := items.Query(t.Context(), store.ItemQuery{IncludeItemTypes: []item.Kind{item.KindMusicArtist}})
	if err != nil {
		t.Fatalf("query artists: %v", err)
	}
	result := map[string]item.Item{}
	for _, value := range page.Items {
		if value.Name == nil {
			t.Fatalf("artist %s has no name", value.ID)
		}
		if _, found := result[*value.Name]; found {
			t.Fatalf("duplicate artist %q", *value.Name)
		}
		result[*value.Name] = value
	}
	return result
}

func sortedNames(values map[string]item.Item) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func artistEndpointNames(t *testing.T, routes *router.Router, path string) []string {
	t.Helper()
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path+"?SortBy=SortName&EnableImages=false&EnableUserData=false", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
	}
	var page struct{ Items []struct{ Name string } }
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	names := make([]string, len(page.Items))
	for index, value := range page.Items {
		names[index] = value.Name
	}
	return names
}
