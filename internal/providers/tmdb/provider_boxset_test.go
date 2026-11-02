// SPDX-License-Identifier: GPL-2.0-only

package tmdb_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
)

const collectionCassette = "testdata/collection.json"

func newCollectionProvider(t *testing.T) (*tmdb.Provider, *recordingDoer) {
	t.Helper()
	cassette, err := remotetest.Load(collectionCassette)
	if err != nil {
		t.Fatalf("load cassette: %v", err)
	}
	doer := &recordingDoer{inner: cassette}
	return newTVProvider(t, doer, nil), doer
}

func boxSetItem(name, tmdbID string) item.Item {
	target := item.Item{Kind: item.KindBoxSet, Name: stringPtr(name)}
	if tmdbID != "" {
		target.ProviderIDs.Set("Tmdb", tmdbID)
	}
	return target
}

// TestRefreshBoxSetMapsTMDbCollection ports TmdbBoxSetProvider.GetMetadata
// and TmdbBoxSetImageProvider.GetImages for a BoxSet with a TMDb id
// (deadfin#808): Name, Overview, the first poster as Primary, the first
// untagged backdrop as Backdrop, and the first language-tagged backdrop as
// Thumb.
func TestRefreshBoxSetMapsTMDbCollection(t *testing.T) {
	provider, doer := newCollectionProvider(t)
	target := boxSetItem("Cars Collection", "87118")

	handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target)
	if err != nil || !handled {
		t.Fatalf("Refresh = %v, %v; want handled", handled, err)
	}
	if target.Overview == nil || *target.Overview != "Animated films about racing cars." {
		t.Errorf("Overview = %v, want the collection overview", target.Overview)
	}
	if got, _ := target.ProviderIDs.Get("Tmdb"); got != "87118" {
		t.Errorf("Tmdb id = %q, want 87118", got)
	}
	want := []item.ImageInfo{
		{Type: item.ImageTypePrimary, Path: "https://image.tmdb.org/t/p/original/cars-poster.jpg", Width: 1000, Height: 1500},
		{Type: item.ImageTypeBackdrop, Path: "https://image.tmdb.org/t/p/original/cars-backdrop.jpg", Width: 1920, Height: 1080},
		{Type: item.ImageTypeThumb, Path: "https://image.tmdb.org/t/p/original/cars-title.jpg", Width: 1280, Height: 720},
	}
	if !reflect.DeepEqual(target.ImageInfos, want) {
		t.Errorf("ImageInfos = %#v, want %#v", target.ImageInfos, want)
	}
	if paths := doer.apiPaths(); !reflect.DeepEqual(paths, []string{"/3/collection/87118"}) {
		t.Errorf("requests = %v, want one collection request", paths)
	}
}

// TestRefreshBoxSetSearchesByName ports the GetMetadata fallback: without a
// TMDb id the first /search/collection result for the cleaned name supplies
// it. A clean miss leaves the BoxSet unhandled.
func TestRefreshBoxSetSearchesByName(t *testing.T) {
	provider, doer := newCollectionProvider(t)
	target := boxSetItem("Cars Collection", "")
	if handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &target); err != nil || !handled {
		t.Fatalf("Refresh = %v, %v; want handled", handled, err)
	}
	if got, _ := target.ProviderIDs.Get("Tmdb"); got != "87118" {
		t.Errorf("Tmdb id = %q, want 87118 from the search", got)
	}
	if paths := doer.apiPaths(); !reflect.DeepEqual(paths, []string{"/3/search/collection", "/3/collection/87118"}) {
		t.Errorf("requests = %v, want search then collection", paths)
	}

	missing := boxSetItem("No Such Collection", "")
	if handled, err := provider.Refresh(t.Context(), movieRefreshContext(), &missing); err != nil || handled {
		t.Fatalf("Refresh miss = %v, %v; want unhandled", handled, err)
	}
	if len(missing.ImageInfos) != 0 {
		t.Errorf("miss images = %v, want none", missing.ImageInfos)
	}
}

// TestRefreshBoxSetKeepsStoredPrimary checks that a refresh without
// replace-all fills only missing image types.
func TestRefreshBoxSetKeepsStoredPrimary(t *testing.T) {
	provider, _ := newCollectionProvider(t)
	target := boxSetItem("Cars Collection", "87118")
	target.ImageInfos = []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "/collections/Cars Collection [boxset]/poster.jpg"}}
	if _, err := provider.Refresh(t.Context(), movieRefreshContext(), &target); err != nil {
		t.Fatal(err)
	}
	if target.ImageInfos[0].Path != "/collections/Cars Collection [boxset]/poster.jpg" || len(target.ImageInfos) != 3 {
		t.Errorf("ImageInfos = %#v, want the stored Primary and the remote Backdrop and Thumb", target.ImageInfos)
	}
}

// TestLookupBoxSetImages ports TmdbBoxSetImageProvider for GET
// /Items/{id}/RemoteImages: every poster and backdrop of the collection.
func TestLookupBoxSetImages(t *testing.T) {
	provider := newCassetteLookupProvider(t, collectionCassette)
	if !provider.ImageSupports(item.KindBoxSet) {
		t.Fatal("ImageSupports(BoxSet) = false")
	}
	if got, want := provider.ImageTypes(item.KindBoxSet), []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop, item.ImageTypeThumb}; !reflect.DeepEqual(got, want) {
		t.Errorf("ImageTypes = %v, want %v", got, want)
	}
	images, err := provider.Images(context.Background(), providers.ImageLookup{Kind: item.KindBoxSet, ProviderIDs: map[string]string{"Tmdb": "87118"}})
	if err != nil {
		t.Fatalf("Images: %v", err)
	}
	if len(images) != 4 {
		t.Fatalf("images = %#v, want 2 posters and 2 backdrops", images)
	}
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/cars-poster.jpg", 1000, 1500)
	assertRemoteImageDimensions(t, images, item.ImageTypePrimary, "https://image.tmdb.org/t/p/original/cars-poster-2.jpg", 800, 1200)
	assertRemoteImageDimensions(t, images, item.ImageTypeBackdrop, "https://image.tmdb.org/t/p/original/cars-backdrop.jpg", 1920, 1080)
	assertRemoteImageDimensions(t, images, item.ImageTypeThumb, "https://image.tmdb.org/t/p/original/cars-title.jpg", 1280, 720)

	none, err := provider.Images(context.Background(), providers.ImageLookup{Kind: item.KindBoxSet})
	if err != nil || len(none) != 0 {
		t.Errorf("Images without a Tmdb id = %v, %v; want none", none, err)
	}
}
