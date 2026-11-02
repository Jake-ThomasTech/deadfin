// SPDX-License-Identifier: GPL-2.0-only

package trickplay_test

import (
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/trickplay"
)

type readerMediaPaths map[dto.UUID]string

func (paths readerMediaPaths) MediaPath(_ context.Context, itemID dto.UUID) (string, error) {
	return paths[itemID], nil
}

func TestMediaDirectoryChangesTheFileExtension(t *testing.T) {
	for path, want := range map[string]string{
		"/media/tv/Show/Show - S00E01 - Pilot HDTV-1080p.m4v": "/media/tv/Show/Show - S00E01 - Pilot HDTV-1080p.trickplay",
		"/media/movies/Heat (1995)/Heat.part1.mkv":            "/media/movies/Heat (1995)/Heat.part1.trickplay",
		"/media/movies/Heat (1995)/VIDEO_TS":                  "/media/movies/Heat (1995)/VIDEO_TS.trickplay",
	} {
		if got := trickplay.MediaDirectory(path); got != want {
			t.Errorf("MediaDirectory(%q) = %q, want %q", path, got, want)
		}
	}
}

// The reporter's case (#806): a 95-minute episode with six 10x10 tiles of a
// 320-wide rendition and the default 10-second interval.
func TestDiscoveredKeepsConfiguredIntervalThatFitsTheRuntime(t *testing.T) {
	runtime := int64(5_700_000) * 10_000
	got, err := (trickplay.Manager{}).Discovered(trickplay.DiscoveredRequest{
		ItemID: dto.UUID{0: 1}, Width: 320, TileWidth: 10, TileHeight: 10, RunTimeTicks: &runtime,
		IntervalMilliseconds: 10_000, FirstTileHeight: 1800,
		TileByteSizes: []int64{400_000, 410_000, 405_000, 401_000, 399_000, 300_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := trickplay.Metadata{
		ItemID: dto.UUID{0: 1}, Width: 320, Height: 180, TileWidth: 10, TileHeight: 10,
		ThumbnailCount: 570, Interval: 10_000,
		// ceil(410000 * 8 / 10 / 10 / 10)
		Bandwidth: 3280,
	}
	if got != want {
		t.Fatalf("Discovered() = %+v, want %+v", got, want)
	}
}

func TestDiscoveredSnapsAnIntervalThatDoesNotFit(t *testing.T) {
	runtime := int64(600_000) * 10_000
	got, err := (trickplay.Manager{}).Discovered(trickplay.DiscoveredRequest{
		ItemID: dto.UUID{0: 1}, Width: 320, TileWidth: 10, TileHeight: 10, RunTimeTicks: &runtime,
		IntervalMilliseconds: 30_000, FirstTileHeight: 1795, TileByteSizes: []int64{1, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 200 thumbnails at most, 101 at least: 3000..5940 ms, midpoint 4470
	// snaps to 4000; 600 s / 4 s = 150 thumbnails.
	if got.Interval != 4000 || got.ThumbnailCount != 150 || got.Height != 180 {
		t.Fatalf("Discovered() interval %d, count %d, height %d; want 4000, 150, 180", got.Interval, got.ThumbnailCount, got.Height)
	}
}

func TestDiscoveredWithoutRuntimeFillsEveryTile(t *testing.T) {
	got, err := (trickplay.Manager{}).Discovered(trickplay.DiscoveredRequest{
		ItemID: dto.UUID{0: 1}, Width: 320, TileWidth: 10, TileHeight: 10,
		IntervalMilliseconds: 500, FirstTileHeight: 1800, TileByteSizes: []int64{1, 1, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Interval != 1000 || got.ThumbnailCount != 300 {
		t.Fatalf("Discovered() interval %d, count %d; want 1000, 300", got.Interval, got.ThumbnailCount)
	}
}

func TestDiscoverMediaCataloguesResolutionDirectoriesWithoutRows(t *testing.T) {
	root := t.TempDir()
	media := filepath.Join(root, "Show", "Show - S00E01.m4v")
	trickplayDir := filepath.Join(root, "Show", "Show - S00E01.trickplay")
	writeDiscoverTile(t, filepath.Join(trickplayDir, "320 - 10x10", "0.jpg"), 320, 1800)
	writeDiscoverTile(t, filepath.Join(trickplayDir, "320 - 10x10", "1.jpg"), 320, 1800)
	writeDiscoverFile(t, filepath.Join(trickplayDir, "320 - 10x10", "notes.txt"))
	writeDiscoverTile(t, filepath.Join(trickplayDir, "640 - 10x10", "0.jpg"), 640, 3600)
	writeDiscoverFile(t, filepath.Join(trickplayDir, "480 - 10x10", "notes.txt"))
	writeDiscoverTile(t, filepath.Join(trickplayDir, "backup", "0.jpg"), 320, 1800)
	runtime := int64(1_500_000) * 10_000

	got, err := trickplay.DiscoverMedia(t.Context(), trickplay.DiscoverRequest{
		ItemID: dto.UUID{0: 1}, MediaPath: media, RunTimeTicks: &runtime,
		IntervalMilliseconds: 10_000, ExistingWidths: []int{640},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("DiscoverMedia() = %+v, want only the 320 rendition", got)
	}
	if want := (trickplay.Metadata{ItemID: dto.UUID{0: 1}, Width: 320, Height: 180, TileWidth: 10, TileHeight: 10, ThumbnailCount: 150, Interval: 10_000, Bandwidth: got[0].Bandwidth}); got[0] != want || got[0].Bandwidth <= 0 {
		t.Fatalf("DiscoverMedia() = %+v, want %+v with positive bandwidth", got[0], want)
	}
	present, err := trickplay.MediaResolutionHasTiles(media, 320, 10, 10)
	if err != nil || !present {
		t.Fatalf("MediaResolutionHasTiles(320) = %v, %v; want true", present, err)
	}
	present, err = trickplay.MediaResolutionHasTiles(media, 480, 10, 10)
	if err != nil || present {
		t.Fatalf("MediaResolutionHasTiles(480) = %v, %v; want false", present, err)
	}
}

func TestDiscoverMediaWithoutTrickplayDirectoryFindsNothing(t *testing.T) {
	got, err := trickplay.DiscoverMedia(t.Context(), trickplay.DiscoverRequest{
		ItemID: dto.UUID{0: 1}, MediaPath: filepath.Join(t.TempDir(), "Heat.mkv"), IntervalMilliseconds: 10_000,
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("DiscoverMedia() = %+v, %v; want none", got, err)
	}
}

// A rendition with no tiles in the owned root is served from the directory
// saved with the media (TrickplayController with SaveTrickplayWithMedia).
func TestReaderServiceFallsBackToTilesSavedWithMedia(t *testing.T) {
	itemID := readerUUID(t, "b6d1efb1-7908-4e4e-92ce-8fa5d40f8b4f")
	metadata := readerMetadata(itemID, 320, 5)
	media := filepath.Join(t.TempDir(), "Movie (2001)", "Movie.mkv")
	tile := filepath.Join(trickplay.MediaDirectory(media), fmt.Sprintf("%d - %dx%d", metadata.Width, metadata.TileWidth, metadata.TileHeight), "0.jpg")
	writeDiscoverTile(t, tile, 320, 1800)
	want, err := os.ReadFile(tile)
	if err != nil {
		t.Fatal(err)
	}

	service := trickplay.NewReaderService(trickplay.ReaderServiceConfig{
		Metadata: &readerMetadataSource{metadata: metadata}, Access: readerAllowAccess{}, Publication: readerPublication(),
		Root: t.TempDir(), Media: readerMediaPaths{itemID: media},
	})
	defer service.Close()
	artifact, err := service.Tile(t.Context(), trickplay.ReadRequest{ItemID: itemID, Width: 320}, 0)
	if err != nil {
		t.Fatalf("Tile(): %v", err)
	}
	defer artifact.Body.Close()
	got, err := io.ReadAll(artifact.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || artifact.GenerationToken != metadata.GenerationToken {
		t.Fatalf("Tile() = %d bytes, token %q; want the media-side tile and token %q", len(got), artifact.GenerationToken, metadata.GenerationToken)
	}
	if _, err := service.Tile(t.Context(), trickplay.ReadRequest{ItemID: itemID, Width: 320}, 1); err == nil {
		t.Fatal("Tile(1) succeeded, want not found for a missing media-side tile")
	}
}

func writeDiscoverTile(t *testing.T, path string, width, height int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, image.NewGray(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 50}); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeDiscoverFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
