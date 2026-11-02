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
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestExistingMusicLibraryScanExtractsEmbeddedCovers covers issue #804. The
// library is scanned while the Image Extractor is off, as a library scanned
// before #784 was. The next plain scan gives each existing track its
// embedded cover, and the album takes the cover of its first track in the
// same scan (D-117). A further scan changes nothing.
func TestExistingMusicLibraryScanExtractsEmbeddedCovers(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("embedded audio covers require ffmpeg: %v", err)
	}
	programData := filepath.Join(t.TempDir(), "program-data")
	paths, err := config.NewPaths(programData)
	if err != nil {
		t.Fatalf("resolve program-data paths: %v", err)
	}
	front := filepath.Join(t.TempDir(), "front.jpg")
	runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "color=c=blue:size=300x300", "-frames:v", "1", front)
	musicRoot := t.TempDir()
	album := filepath.Join(musicRoot, "Artist", "Album")
	tracks := []string{filepath.Join(album, "01 - First.flac"), filepath.Join(album, "02 - Second.flac")}
	for number, track := range tracks {
		writeCoverTrack(t, ffmpegPath, track, front, number)
	}

	app := newCompositionApplication(t, programData)
	folder, err := app.topology.AddVirtualFolder(t.Context(), "Music", "music", library.LibraryOptions{
		PathInfos:   []library.MediaPathInfo{{Path: musicRoot}},
		TypeOptions: offlineMusicTypeOptions(),
	})
	if err != nil {
		t.Fatalf("add music library: %v", err)
	}
	scanMusicLibrary(t, app, library.DefaultRefreshOptions())
	items := store.NewItemStore(app.stores)
	for _, path := range append([]string{album}, tracks...) {
		kind := item.KindAudio
		if path == album {
			kind = item.KindMusicAlbum
		}
		if images := applicationScannedItem(t, items, path, kind).ImageInfos; len(images) != 0 {
			t.Fatalf("images of %s with the Image Extractor off = %+v, want none", path, images)
		}
	}

	if err := app.topology.UpdateLibraryOptions(t.Context(), folder.CollectionFolderID, library.LibraryOptions{
		PathInfos:   []library.MediaPathInfo{{Path: musicRoot}},
		TypeOptions: offlineMusicTypeOptions(providers.AudioImageExtractorName),
	}); err != nil {
		t.Fatalf("turn the Image Extractor on: %v", err)
	}
	scanMusicLibrary(t, app, library.DefaultRefreshOptions())
	scanned := map[string]item.Kind{tracks[0]: item.KindAudio, tracks[1]: item.KindAudio, album: item.KindMusicAlbum}
	before := make(map[string][]item.ImageInfo, len(scanned))
	for path, kind := range scanned {
		value := applicationScannedItem(t, items, path, kind)
		assertFreshPrimaryImage(t, paths.Metadata, value, 300, 300)
		before[path] = value.ImageInfos
	}
	files := metadataImageFiles(t, paths.Metadata)

	scanMusicLibrary(t, app, library.DefaultRefreshOptions())
	for path, kind := range scanned {
		if value := applicationScannedItem(t, items, path, kind); !reflect.DeepEqual(value.ImageInfos, before[path]) {
			t.Fatalf("rescan images of %s = %+v, want %+v", path, value.ImageInfos, before[path])
		}
	}
	if after := metadataImageFiles(t, paths.Metadata); !reflect.DeepEqual(after, files) {
		t.Fatalf("rescan metadata image files = %q, want %q", after, files)
	}
}

// TestReplaceAllImagesKeepsRemoteAlbumCover covers issue #804. An album
// whose tracks have no embedded cover keeps its remote Primary image on a
// refresh that replaces all images, because no provider supplies another
// one (ItemImageProvider.RefreshImages).
func TestReplaceAllImagesKeepsRemoteAlbumCover(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("music fixtures require ffmpeg: %v", err)
	}
	programData := filepath.Join(t.TempDir(), "program-data")
	musicRoot := t.TempDir()
	album := filepath.Join(musicRoot, "Artist", "Album")
	track := filepath.Join(album, "01 - First.flac")
	runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "flac",
		"-metadata", "title=Track 1", "-metadata", "album=Album", "-metadata", "album_artist=Artist", mkdirFor(t, track))

	app := newCompositionApplication(t, programData)
	if _, err := app.topology.AddVirtualFolder(t.Context(), "Music", "music", library.LibraryOptions{
		PathInfos:   []library.MediaPathInfo{{Path: musicRoot}},
		TypeOptions: offlineMusicTypeOptions(providers.AudioImageExtractorName),
	}); err != nil {
		t.Fatalf("add music library: %v", err)
	}
	scanMusicLibrary(t, app, library.DefaultRefreshOptions())
	items := store.NewItemStore(app.stores)
	stored := applicationScannedItem(t, items, album, item.KindMusicAlbum)
	cover := item.ImageInfo{Type: item.ImageTypePrimary, Path: "https://coverartarchive.org/release-group/0000/front.jpg"}
	stored.ImageInfos = []item.ImageInfo{cover}
	if err := items.EditItems(t.Context(), []item.Item{stored}, nil); err != nil {
		t.Fatalf("store remote album cover: %v", err)
	}

	scanMusicLibrary(t, app, library.RefreshOptions{
		Metadata:         library.RefreshModeFullRefresh,
		Image:            library.RefreshModeFullRefresh,
		ReplaceAllImages: true,
		Targets:          []library.RefreshTarget{{ItemID: stored.ID, Path: album}},
	})
	if got := applicationScannedItem(t, items, album, item.KindMusicAlbum).ImageInfos; !reflect.DeepEqual(got, []item.ImageInfo{cover}) {
		t.Fatalf("album images after ReplaceAllImages = %+v, want %+v", got, []item.ImageInfo{cover})
	}
}

// offlineMusicTypeOptions turns off the remote metadata and image providers
// of the music kinds, so the scan makes no network request. audioImages
// lists the image fetchers of Audio.
func offlineMusicTypeOptions(audioImages ...string) []library.TypeOptions {
	return []library.TypeOptions{
		{Type: "MusicArtist", MetadataFetchers: []string{}, ImageFetchers: []string{}},
		{Type: "MusicAlbum", MetadataFetchers: []string{}, ImageFetchers: []string{}},
		{Type: "Audio", MetadataFetchers: []string{}, ImageFetchers: append([]string{}, audioImages...)},
	}
}

func writeCoverTrack(t *testing.T, ffmpegPath, track, cover string, number int) {
	t.Helper()
	runFixtureFFmpeg(t, ffmpegPath, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-i", cover,
		"-map", "0:a", "-map", "1", "-c:a", "flac", "-c:v", "copy", "-disposition:v:0", "attached_pic",
		"-metadata", "title=Track "+string(rune('1'+number)), "-metadata", "album=Album", "-metadata", "album_artist=Artist",
		"-metadata", "track="+string(rune('1'+number)), mkdirFor(t, track))
}

func scanMusicLibrary(t *testing.T, app *application, options library.RefreshOptions) {
	t.Helper()
	result, err := app.scanner.ScanWithOptions(t.Context(), options)
	if err != nil {
		t.Fatalf("scan music library: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("scan errors = %v", result.Errors)
	}
}
