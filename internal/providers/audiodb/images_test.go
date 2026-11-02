// SPDX-License-Identifier: GPL-2.0-only

package audiodb_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/audiodb"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
)

// TestImageListerMapsArtistAndAlbumImagesFromCassette covers issue #807:
// AudioDbArtistImageProvider and AudioDbAlbumImageProvider list the remote
// images of an artist by its MusicBrainzArtist ID and of an album by its
// MusicBrainzReleaseGroup ID.
func TestImageListerMapsArtistAndAlbumImagesFromCassette(t *testing.T) {
	cassette, err := remotetest.Load(filepath.Join("testdata", "supplemental-metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	doer := &recordingDoer{inner: cassette}
	config := audiodb.DefaultConfig()
	config.APIKey = "test-key"
	provider := audiodb.NewProvider(audiodb.Options{HTTP: remote.NewClient(remote.Config{Doer: doer}), Config: config})
	var lister providers.ImageLister = provider

	for kind, want := range map[item.Kind][]item.ImageType{
		item.KindMusicArtist: {item.ImageTypePrimary, item.ImageTypeLogo, item.ImageTypeBanner, item.ImageTypeBackdrop},
		item.KindMusicAlbum:  {item.ImageTypePrimary, item.ImageTypeDisc},
		item.KindAudio:       nil,
	} {
		if got := lister.ImageSupports(kind); got != (want != nil) {
			t.Fatalf("ImageSupports(%s) = %t, want %t", kind, got, want != nil)
		}
		if got := lister.ImageTypes(kind); !reflect.DeepEqual(got, want) {
			t.Fatalf("ImageTypes(%s) = %v, want %v", kind, got, want)
		}
	}

	album, err := lister.Images(t.Context(), providers.ImageLookup{
		Kind:        item.KindMusicAlbum,
		ProviderIDs: map[string]string{"MusicBrainzAlbum": "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "MusicBrainzReleaseGroup": groupID},
	})
	if err != nil {
		t.Fatalf("album images: %v", err)
	}
	wantAlbum := []providers.RemoteImageInfo{
		{ProviderName: "TheAudioDB", URL: "https://images.example/album-thumb.jpg", Type: item.ImageTypePrimary},
		{ProviderName: "TheAudioDB", URL: "https://images.example/album-disc.png", Type: item.ImageTypeDisc},
	}
	if !reflect.DeepEqual(album, wantAlbum) {
		t.Fatalf("album images = %#v, want %#v", album, wantAlbum)
	}

	artist, err := lister.Images(t.Context(), providers.ImageLookup{
		Kind:        item.KindMusicArtist,
		ProviderIDs: map[string]string{"MusicBrainzArtist": artistID},
	})
	if err != nil {
		t.Fatalf("artist images: %v", err)
	}
	wantArtist := []providers.RemoteImageInfo{
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-thumb.jpg", Type: item.ImageTypePrimary},
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-logo.png", Type: item.ImageTypeLogo},
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-banner.jpg", Type: item.ImageTypeBanner},
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-fanart-1.jpg", Type: item.ImageTypeBackdrop},
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-fanart-2.jpg", Type: item.ImageTypeBackdrop},
		{ProviderName: "TheAudioDB", URL: "https://images.example/artist-fanart-3.jpg", Type: item.ImageTypeBackdrop},
	}
	if !reflect.DeepEqual(artist, wantArtist) {
		t.Fatalf("artist images = %#v, want %#v", artist, wantArtist)
	}

	// An album without a release-group ID has no images and sends no
	// request (AudioDbAlbumImageProvider.GetImages).
	requests := len(doer.snapshot())
	if images, err := lister.Images(t.Context(), providers.ImageLookup{
		Kind:        item.KindMusicAlbum,
		ProviderIDs: map[string]string{"MusicBrainzAlbum": "dddddddd-dddd-4ddd-8ddd-dddddddddddd"},
	}); err != nil || images != nil {
		t.Fatalf("album without a release group = %#v, %v; want none", images, err)
	}
	if got := len(doer.snapshot()); got != requests {
		t.Fatalf("album without a release group sent %d requests, want none", got-requests)
	}
}

func TestImageListerKeylessIsInert(t *testing.T) {
	provider := audiodb.NewProvider(audiodb.Options{
		HTTP:   remote.NewClient(remote.Config{Doer: remotetest.NewCassette()}),
		Config: audiodb.DefaultConfig(),
	})
	if provider.ImageSupports(item.KindMusicAlbum) || provider.ImageTypes(item.KindMusicAlbum) != nil {
		t.Fatal("keyless provider lists album images, want inert (D-11)")
	}
	images, err := provider.Images(t.Context(), providers.ImageLookup{
		Kind:        item.KindMusicAlbum,
		ProviderIDs: map[string]string{"MusicBrainzReleaseGroup": groupID},
	})
	if err != nil || images != nil {
		t.Fatalf("keyless album images = %#v, %v; want none", images, err)
	}
}
