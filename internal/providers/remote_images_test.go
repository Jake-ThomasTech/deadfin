// SPDX-License-Identifier: GPL-2.0-only

package providers_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/providers"
)

// fakeLister is an ImageLister that records its calls.
type fakeLister struct {
	name   string
	types  []item.ImageType
	images []providers.RemoteImageInfo
	err    error
	calls  *[]string
}

func (l fakeLister) Name() string                          { return l.name }
func (l fakeLister) ImageSupports(item.Kind) bool          { return true }
func (l fakeLister) ImageTypes(item.Kind) []item.ImageType { return l.types }
func (l fakeLister) Images(_ context.Context, lookup providers.ImageLookup) ([]providers.RemoteImageInfo, error) {
	*l.calls = append(*l.calls, l.name+":"+lookup.ProviderIDs["MusicBrainzReleaseGroup"])
	return l.images, l.err
}

func albumImageOptions(fetchers, order []string) library.LibraryOptions {
	return library.LibraryOptions{TypeOptions: []library.TypeOptions{{
		Type:              "MusicAlbum",
		MetadataFetchers:  []string{"MusicBrainz"},
		ImageFetchers:     fetchers,
		ImageFetcherOrder: order,
	}}}
}

func primaryPaths(target item.Item) []string {
	var paths []string
	for _, image := range target.ImageInfos {
		paths = append(paths, image.Type.String()+"="+image.Path)
	}
	return paths
}

// TestRegistryRemoteImagesFillMissingPrimary covers deadfin#847: an image
// refresh runs the enabled image fetchers in ImageFetcherOrder and fills a
// missing Primary image, as upstream ItemImageProvider.RefreshImages runs
// the IRemoteImageProvider image fetchers. The image fetcher runs although
// the library's MetadataFetchers do not name it. A disabled fetcher and an
// image type with limit 0 (album Disc) are not fetched.
func TestRegistryRemoteImagesFillMissingPrimary(t *testing.T) {
	var calls []string
	audioDB := fakeLister{name: "TheAudioDB", types: []item.ImageType{item.ImageTypePrimary, item.ImageTypeDisc}, calls: &calls, images: []providers.RemoteImageInfo{
		{URL: "https://images.example/disc.png", Type: item.ImageTypeDisc},
		{URL: "https://images.example/cover-fr.jpg", Type: item.ImageTypePrimary, Language: "fr"},
		{URL: "https://images.example/cover.jpg", Type: item.ImageTypePrimary},
	}}
	other := fakeLister{name: "Other", types: []item.ImageType{item.ImageTypePrimary}, calls: &calls, images: []providers.RemoteImageInfo{
		{URL: "https://images.example/other.jpg", Type: item.ImageTypePrimary},
	}}
	disabled := fakeLister{name: "Disabled", types: []item.ImageType{item.ImageTypePrimary}, calls: &calls}
	registry := providers.NewRegistry().WithImageListers(other, disabled, audioDB)

	target := item.Item{Kind: item.KindMusicAlbum}
	target.ProviderIDs.Set("MusicBrainzReleaseGroup", "group")
	refresh := library.RefreshContext{
		Mode:            library.RefreshModeFullRefresh,
		Image:           library.RefreshModeFullRefresh,
		RunRemoteImages: true,
		Options:         albumImageOptions([]string{"TheAudioDB", "Other"}, []string{"TheAudioDB", "Other"}),
	}
	outcome, err := registry.RefreshWithOutcome(t.Context(), refresh, &target)
	if err != nil || outcome.RemoteFailures != 0 {
		t.Fatalf("refresh = %+v, %v", outcome, err)
	}
	if want := []string{"Primary=https://images.example/cover.jpg"}; !reflect.DeepEqual(primaryPaths(target), want) {
		t.Errorf("images = %v, want %v", primaryPaths(target), want)
	}
	if want := []string{"TheAudioDB:group"}; !reflect.DeepEqual(calls, want) {
		t.Errorf("fetcher calls = %v, want %v (Other is not asked once Primary is set)", calls, want)
	}
}

// TestRegistryRemoteImagesGates proves when the remote image stage does not
// fetch: without RunRemoteImages, at ValidationOnly, for a locked item
// below FullRefresh, for a type the stored item has, and for a fetcher whose
// metadata provider handled the item. A replace-all image refresh ignores
// the stored image. A fetcher failure is counted and logged.
func TestRegistryRemoteImagesGates(t *testing.T) {
	cover := []providers.RemoteImageInfo{{URL: "https://images.example/cover.jpg", Type: item.ImageTypePrimary}}
	stored := []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "%MetadataPath%/library/aa/primary.jpg"}}
	for _, testCase := range []struct {
		name       string
		refresh    library.RefreshContext
		locked     bool
		handled    bool
		err        error
		wantImages int
		wantFails  int
	}{
		{name: "full refresh fills", refresh: library.RefreshContext{Image: library.RefreshModeFullRefresh, RunRemoteImages: true}, wantImages: 1},
		{name: "not a full or first refresh", refresh: library.RefreshContext{Image: library.RefreshModeDefault}},
		{name: "validation only", refresh: library.RefreshContext{Image: library.RefreshModeValidationOnly, RunRemoteImages: true}},
		{name: "locked below full refresh", refresh: library.RefreshContext{Image: library.RefreshModeDefault, RunRemoteImages: true}, locked: true},
		{name: "locked on full refresh", refresh: library.RefreshContext{Image: library.RefreshModeFullRefresh, RunRemoteImages: true}, locked: true, wantImages: 1},
		{name: "stored image", refresh: library.RefreshContext{Image: library.RefreshModeFullRefresh, RunRemoteImages: true, StoredImages: stored}},
		{name: "replace all ignores stored image", refresh: library.RefreshContext{Image: library.RefreshModeFullRefresh, ReplaceAllImages: true, RunRemoteImages: true, StoredImages: stored}, wantImages: 1},
		{name: "metadata provider handled", refresh: library.RefreshContext{Mode: library.RefreshModeFullRefresh, Image: library.RefreshModeFullRefresh, RunRemoteImages: true}, handled: true},
		{name: "fetcher failure", refresh: library.RefreshContext{Image: library.RefreshModeFullRefresh, RunRemoteImages: true}, err: errors.New("boom"), wantFails: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls []string
			lister := fakeLister{name: "TheAudioDB", types: []item.ImageType{item.ImageTypePrimary}, images: cover, err: testCase.err, calls: &calls}
			var metadata []providers.Provider
			if testCase.handled {
				metadata = append(metadata, providerFunc{name: "TheAudioDB", stage: providers.ProviderStageRemoteMetadata, refresh: func(context.Context, library.RefreshContext, *item.Item) (bool, error) {
					return true, nil
				}})
			}
			registry := providers.NewRegistry(metadata...).WithImageListers(lister)
			refresh := testCase.refresh
			refresh.Options = albumImageOptions([]string{"TheAudioDB"}, nil)
			refresh.Options.TypeOptions[0].MetadataFetchers = []string{"TheAudioDB"}
			target := item.Item{Kind: item.KindMusicAlbum, IsLocked: testCase.locked}
			outcome, err := registry.RefreshWithOutcome(t.Context(), refresh, &target)
			if err != nil {
				t.Fatal(err)
			}
			if len(target.ImageInfos) != testCase.wantImages {
				t.Errorf("images = %v, want %d", primaryPaths(target), testCase.wantImages)
			}
			if outcome.RemoteFailures != testCase.wantFails {
				t.Errorf("remote failures = %d, want %d", outcome.RemoteFailures, testCase.wantFails)
			}
		})
	}
}
