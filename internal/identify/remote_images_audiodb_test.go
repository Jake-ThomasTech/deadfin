// SPDX-License-Identifier: GPL-2.0-only

package identify

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/audiodb"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
	"github.com/deadfin-org/deadfin/internal/providers/remote/remotetest"
)

// TestRemoteImagesListsTheAudioDBForAnAlbum covers issue #807: an album with
// a MusicBrainz release-group ID lists TheAudioDB as its image provider and
// its album thumb as a Primary image (AudioDbAlbumImageProvider).
func TestRemoteImagesListsTheAudioDBForAnAlbum(t *testing.T) {
	items := setupTestItems(t)
	album := item.Item{
		ID:          mustUUID("807a0b0c0d0e4f00a1b2c3d4e5f60708"),
		Kind:        item.KindMusicAlbum,
		Name:        ptr("4"),
		ProviderIDs: newProviderIDs("MusicBrainzReleaseGroup", "1c3d2b6e-5d8a-4c2e-9f7b-0a1b2c3d4e5f"),
	}
	if err := items.Upsert(context.Background(), album); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	cassette := remotetest.NewCassette(remotetest.Interaction{
		Method: http.MethodGet,
		URL:    "https://www.theaudiodb.com/api/v1/json/test-key/album-mb.php?i=1c3d2b6e-5d8a-4c2e-9f7b-0a1b2c3d4e5f",
		Status: http.StatusOK,
		Body:   []byte(`{"album":[{"idAlbum":"2115888","strAlbum":"4","strAlbumThumb":"https://r2.theaudiodb.com/images/media/album/thumb/4.jpg","strAlbumCDart":null}]}`),
	})
	config := audiodb.DefaultConfig()
	config.APIKey = "test-key"
	provider := audiodb.NewProvider(audiodb.Options{HTTP: remote.NewClient(remote.Config{Doer: cassette}), Config: config})
	svc := New(Options{
		Items:   items,
		Lookups: providers.NewLookupRegistry(nil, []providers.ImageLister{provider}),
		Clock:   clock.NewFake(identifyTestInstant()),
	})

	primary := item.ImageTypePrimary
	result, err := svc.RemoteImages(context.Background(), RemoteImagesQuery{ItemID: album.ID, Type: &primary})
	if err != nil {
		t.Fatalf("RemoteImages: %v", err)
	}
	if !reflect.DeepEqual(result.Providers, []string{"TheAudioDB"}) {
		t.Fatalf("providers = %v, want [TheAudioDB]", result.Providers)
	}
	want := []providers.RemoteImageInfo{{ProviderName: "TheAudioDB", URL: "https://r2.theaudiodb.com/images/media/album/thumb/4.jpg", Type: item.ImageTypePrimary}}
	if result.TotalRecordCount != 1 || !reflect.DeepEqual(result.Images, want) {
		t.Fatalf("images = %d %#v, want %#v", result.TotalRecordCount, result.Images, want)
	}

	infos, err := svc.RemoteImageProviders(context.Background(), album.ID)
	if err != nil {
		t.Fatalf("RemoteImageProviders: %v", err)
	}
	wantInfos := []ImageProviderInfo{{Name: "TheAudioDB", SupportedImages: []item.ImageType{item.ImageTypePrimary, item.ImageTypeDisc}}}
	if !reflect.DeepEqual(infos, wantInfos) {
		t.Fatalf("image providers = %#v, want %#v", infos, wantInfos)
	}
}
