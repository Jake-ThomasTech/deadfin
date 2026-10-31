// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestFreshMusicLibraryScanExtractsEmbeddedCovers covers issue #784. Each
// FLAC track carries its front cover as an attached picture and the album
// folder has no cover file. The Image Extractor gives each track a Primary
// image, and the album takes the Primary image of its first track. A rescan
// of the unchanged files changes nothing.
func TestFreshMusicLibraryScanExtractsEmbeddedCovers(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("embedded audio covers require ffmpeg: %v", err)
	}
	programData := filepath.Join(t.TempDir(), "program-data")
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatalf("resolve program-data paths: %v", err)
	}
	fixtures := t.TempDir()
	front := filepath.Join(fixtures, "front.jpg")
	runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "color=c=blue:size=300x300", "-frames:v", "1", front)
	back := filepath.Join(fixtures, "back.png")
	runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "color=c=green:size=120x80", "-frames:v", "1", back)

	musicRoot := t.TempDir()
	album := filepath.Join(musicRoot, "Artist", "Album")
	tracks := []string{filepath.Join(album, "01 - First.flac"), filepath.Join(album, "02 - Second.flac")}
	for number, track := range tracks {
		// The back cover comes first, so the "front" comment selects the
		// stream (AudioImageProvider.GetImage).
		runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-i", back, "-i", front,
			"-map", "0:a", "-map", "1", "-map", "2", "-c:a", "flac", "-c:v", "copy",
			"-disposition:v:0", "attached_pic", "-disposition:v:1", "attached_pic",
			"-metadata:s:v:0", "comment=Cover (back)", "-metadata:s:v:1", "comment=Cover (front)",
			"-metadata", "title=Track "+string(rune('1'+number)), "-metadata", "album=Album", "-metadata", "album_artist=Artist",
			"-metadata", "track="+string(rune('1'+number)), mkdirFor(t, track))
	}

	app := newCompositionApplication(t, programData)
	if _, err := app.topology.AddVirtualFolder(t.Context(), "Music", "music", library.LibraryOptions{
		PathInfos: []library.MediaPathInfo{{Path: musicRoot}},
	}); err != nil {
		t.Fatalf("add music library: %v", err)
	}
	if result, err := app.scanner.Scan(t.Context()); err != nil {
		t.Fatalf("scan fresh music library: %v", err)
	} else if len(result.Errors) != 0 {
		t.Fatalf("scan errors = %v", result.Errors)
	}

	items := store.NewItemStore(app.stores)
	scanned := map[string]item.Kind{tracks[0]: item.KindAudio, tracks[1]: item.KindAudio, album: item.KindMusicAlbum}
	before := make(map[string][]item.ImageInfo, len(scanned))
	for path, kind := range scanned {
		value := applicationScannedItem(t, items, path, kind)
		assertFreshPrimaryImage(t, paths.Metadata, value, 300, 300)
		before[path] = value.ImageInfos
	}
	files := metadataImageFiles(t, paths.Metadata)
	if len(files) != len(scanned) {
		t.Fatalf("metadata image files = %q, want one per track and album", files)
	}

	if result, err := app.scanner.Scan(t.Context()); err != nil {
		t.Fatalf("rescan: %v", err)
	} else if len(result.Errors) != 0 {
		t.Fatalf("rescan errors = %v", result.Errors)
	}
	for path, kind := range scanned {
		value := applicationScannedItem(t, items, path, kind)
		if !reflect.DeepEqual(value.ImageInfos, before[path]) {
			t.Fatalf("rescan images for %s = %+v, want %+v", path, value.ImageInfos, before[path])
		}
	}
	if after := metadataImageFiles(t, paths.Metadata); !reflect.DeepEqual(after, files) {
		t.Fatalf("rescan metadata image files = %q, want %q", after, files)
	}
}
