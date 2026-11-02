// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/jfimport"
	"github.com/deadfin-org/deadfin/internal/store/jellyfinsource"
)

// TestKeepCollectionMetadata proves that an imported collection keeps its
// Jellyfin provider IDs and images (deadfin#808): an image file is copied
// into the collection directory under its local image name, an image URL is
// stored as a remote image, a missing file is a warning, and a rerun
// changes nothing.
func TestKeepCollectionMetadata(t *testing.T) {
	jellyfin := t.TempDir()
	poster := filepath.Join(jellyfin, "poster.JPG")
	if err := os.WriteFile(poster, []byte("poster"), 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "Cars Collection [boxset]")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	collection := item.Item{Kind: item.KindBoxSet, Path: &directory}
	collection.ProviderIDs.Set("Imdb", "kept")
	value := jfimport.CollectionImport{
		Name:        "Cars Collection",
		ProviderIDs: map[string]string{"Tmdb": "87118", "Imdb": "jellyfin"},
		Images: []jellyfinsource.Image{
			{Type: int(item.ImageTypePrimary), Path: poster},
			{Type: int(item.ImageTypeBackdrop), Path: filepath.Join(jellyfin, "missing.jpg")},
			{Type: int(item.ImageTypeThumb), Path: "https://image.tmdb.org/t/p/original/cars.jpg"},
		},
	}

	warnings, changed := keepCollectionMetadata(&collection, value)
	if !changed {
		t.Fatal("changed = false, want true")
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "missing.jpg") {
		t.Errorf("warnings = %v, want one for the missing backdrop", warnings)
	}
	if got, _ := collection.ProviderIDs.Get("Tmdb"); got != "87118" {
		t.Errorf("Tmdb id = %q, want 87118", got)
	}
	if got, _ := collection.ProviderIDs.Get("Imdb"); got != "kept" {
		t.Errorf("Imdb id = %q, want the existing value", got)
	}
	copied := filepath.Join(directory, "poster.jpg")
	if data, err := os.ReadFile(copied); err != nil || string(data) != "poster" {
		t.Fatalf("copied poster = %q, %v", data, err)
	}
	want := []item.ImageInfo{
		{Type: item.ImageTypePrimary, Path: copied},
		{Type: item.ImageTypeThumb, Path: "https://image.tmdb.org/t/p/original/cars.jpg"},
	}
	if !reflect.DeepEqual(collection.ImageInfos, want) {
		t.Errorf("images = %#v, want %#v", collection.ImageInfos, want)
	}

	value.Images = value.Images[:1]
	if warnings, changed := keepCollectionMetadata(&collection, value); changed || len(warnings) != 0 {
		t.Errorf("rerun = %v, %v; want no change", warnings, changed)
	}
}
