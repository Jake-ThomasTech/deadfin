// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
)

func TestScannerRescanKeepsStoredProviderIDsAndImages(t *testing.T) {
	fixture := newScannerSidecarFixture(t)
	scannerScan(t, fixture.harness.scanner, "seed scan")
	movie := scannerItemAtPath(t, fixture.harness.items, fixture.media)

	uploaded := "%MetadataPath%/library/" + movie.ID.String()[:2] + "/" + movie.ID.String() + "/poster-0a1b.jpg"
	remote := "https://image.tmdb.org/t/p/original/backdrop.jpg"
	inLibrary := fixture.directory + "/gone-fanart.jpg"
	movie.ProviderIDs.Set("Tmdb", "603")
	movie.ImageInfos = []item.ImageInfo{
		{Type: item.ImageTypePrimary, Path: uploaded},
		{Type: item.ImageTypeBackdrop, Path: inLibrary},
		{Type: item.ImageTypeBackdrop, Index: 1, Path: remote},
	}
	if err := fixture.harness.items.Upsert(t.Context(), movie); err != nil {
		t.Fatalf("store edited movie: %v", err)
	}

	scannerScan(t, fixture.harness.scanner, "rescan")
	rescanned := scannerItemAtPath(t, fixture.harness.items, fixture.media)
	if value, found := rescanned.ProviderIDs.Get("Tmdb"); !found || value != "603" {
		t.Fatalf("Tmdb provider ID after rescan = %q, %v; want 603", value, found)
	}
	assertScannerImagePaths(t, rescanned, map[string]bool{uploaded: true, remote: true, inLibrary: false})

	// ReplaceAllImages replaces an image only with one that a provider
	// supplies. No provider supplies one here, so both images stay
	// (ItemImageProvider.RefreshImages).
	_, err := fixture.harness.scanner.ScanWithOptions(t.Context(), library.RefreshOptions{
		Metadata:         library.RefreshModeFullRefresh,
		Image:            library.RefreshModeFullRefresh,
		ReplaceAllImages: true,
	})
	if err != nil {
		t.Fatalf("replace-all-images scan: %v", err)
	}
	replaced := scannerItemAtPath(t, fixture.harness.items, fixture.media)
	assertScannerImagePaths(t, replaced, map[string]bool{uploaded: true, remote: true})

	// ReplaceAllMetadata with ReplaceAllImages removes the images in the
	// internal metadata directory (ItemImageProvider.RemoveImages).
	_, err = fixture.harness.scanner.ScanWithOptions(t.Context(), library.RefreshOptions{
		Metadata:           library.RefreshModeFullRefresh,
		Image:              library.RefreshModeFullRefresh,
		ReplaceAllMetadata: true,
		ReplaceAllImages:   true,
	})
	if err != nil {
		t.Fatalf("replace-all scan: %v", err)
	}
	replaced = scannerItemAtPath(t, fixture.harness.items, fixture.media)
	assertScannerImagePaths(t, replaced, map[string]bool{uploaded: false, remote: true})
	if value, found := replaced.ProviderIDs.Get("Tmdb"); !found || value != "603" {
		t.Fatalf("Tmdb provider ID after image replace = %q, %v; want 603", value, found)
	}
}

func assertScannerImagePaths(t *testing.T, value item.Item, want map[string]bool) {
	t.Helper()
	present := make(map[string]bool, len(value.ImageInfos))
	for _, image := range value.ImageInfos {
		present[image.Path] = true
	}
	for path, wanted := range want {
		if present[path] != wanted {
			t.Fatalf("image %q present = %v, want %v; images = %#v", path, present[path], wanted, value.ImageInfos)
		}
	}
}
