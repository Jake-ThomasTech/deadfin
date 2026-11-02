// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/images"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/trickplay"
)

// TestTrickplayTaskDiscoversTrickplaySavedWithMedia covers #806: a library
// imported from Jellyfin with SaveTrickplayWithMedia already has a
// "<name>.trickplay/320 - 10x10" folder next to each video. The scheduled
// task catalogues it, generates nothing, and the reader serves its tiles.
// It runs in a child process for the reason given on
// TestFreshLibraryScanGeneratesTrickplay.
func TestTrickplayTaskDiscoversTrickplaySavedWithMedia(t *testing.T) {
	const childEnvironment = "DEADFIN_TEST_DISCOVER_TRICKPLAY_CHILD"
	if os.Getenv(childEnvironment) != "1" {
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTrickplayTaskDiscoversTrickplaySavedWithMedia$", "-test.v", "-test.count=1")
		command.Env = append(os.Environ(), childEnvironment+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("discover trickplay child: %v\n%s", err, output)
		}
		if strings.Contains(string(output), "--- SKIP: TestTrickplayTaskDiscoversTrickplaySavedWithMedia") {
			t.Skipf("discover trickplay child skipped:\n%s", output)
		}
		return
	}
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("trickplay discovery requires ffmpeg for its fixture: %v", err)
	}
	programData := filepath.Join(t.TempDir(), "program-data")
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatalf("resolve program-data paths: %v", err)
	}
	runtimeOwner, err := images.OpenRuntime(t.Context())
	if err != nil {
		t.Fatalf("open image runtime: %v", err)
	}
	t.Cleanup(func() {
		if err := runtimeOwner.Close(); err != nil {
			t.Errorf("close image runtime: %v", err)
		}
	})
	composer, err := images.NewTileComposer(runtimeOwner, images.TileComposerConfig{Root: paths.Trickplay, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("open tile composer: %v", err)
	}
	t.Cleanup(func() {
		if err := composer.Close(); err != nil {
			t.Errorf("close tile composer: %v", err)
		}
	})

	root := t.TempDir()
	video := writeTrickplayFixtureVideo(t, ffmpegPath, root, "Saved Clip (2024)")
	tilePath := filepath.Join(trickplay.MediaDirectory(video), "320 - 10x10", "0.jpg")
	if err := os.MkdirAll(filepath.Dir(tilePath), 0o755); err != nil {
		t.Fatal(err)
	}
	tile, err := os.Create(tilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(tile, image.NewGray(image.Rect(0, 0, 3200, 1800)), &jpeg.Options{Quality: 50}); err != nil {
		t.Fatal(err)
	}
	if err := tile.Close(); err != nil {
		t.Fatal(err)
	}

	app := newCompositionApplication(t, programData, withTileComposer(composer))
	if _, err := app.topology.AddVirtualFolder(t.Context(), "Movies", "movies", library.LibraryOptions{
		EnableTrickplayImageExtraction: true, SaveTrickplayWithMedia: true,
		PathInfos: []library.MediaPathInfo{{Path: root}},
	}); err != nil {
		t.Fatalf("add library: %v", err)
	}
	if result, err := app.scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	} else if len(result.Errors) != 0 {
		t.Fatalf("scan errors = %v", result.Errors)
	}
	if err := app.trickplayRunner.Wait(t.Context()); err != nil {
		t.Fatalf("wait for scan trickplay: %v", err)
	}
	if err := app.trickplayTask.Run(t.Context(), func(float64) {}); err != nil {
		t.Fatalf("run Generate Trickplay Images: %v", err)
	}

	items := store.NewItemStore(app.stores)
	value := applicationScannedItem(t, items, video, item.KindMovie)
	infos, err := items.Trickplays(t.Context(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 25 s at the configured 10 s interval in one 10x10 tile: 2 thumbnails
	// (Math.Round(2.5) is 2), 1800 / 10 = 180 high.
	if len(infos) != 1 || infos[0].Width != 320 || infos[0].Height != 180 || infos[0].TileWidth != 10 ||
		infos[0].ThumbnailCount != 2 || infos[0].IntervalMS != 10_000 {
		t.Fatalf("trickplay rows = %+v, want the discovered 320 rendition", infos)
	}
	dashed := value.ID.Dashed()
	if _, err := os.Stat(filepath.Join(paths.Trickplay, dashed[:2], dashed)); !os.IsNotExist(err) {
		t.Fatalf("generated trickplay directory: %v; want none", err)
	}
	artifact, err := app.trickplayReader.Tile(t.Context(), trickplay.ReadRequest{ItemID: value.ID, Width: 320, Administrator: true}, 0)
	if err != nil {
		t.Fatalf("read discovered tile: %v", err)
	}
	defer artifact.Body.Close()
	got, err := io.ReadAll(artifact.Body)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(tilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("discovered tile = %d bytes, want the %d-byte tile saved with the media", len(got), len(want))
	}
}
