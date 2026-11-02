// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"reflect"
	"testing"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

func TestCarryStoredImagesCollisions(t *testing.T) {
	const root = "/media/movies"
	upload := "%MetadataPath%/library/ab/abc/poster-0a1b.jpg"
	storedURL := "https://image.tmdb.org/t/p/original/old.jpg"
	providerURL := "https://image.tmdb.org/t/p/original/new.jpg"
	libraryPoster := root + "/Movie/poster.jpg"
	libraryFanart := root + "/Movie/fanart.jpg"
	uploadBackdrop := "%MetadataPath%/library/ab/abc/backdrop-1c2d.jpg"
	storedID, err := dto.ParseUUID("9c3f1d0e7b2a4c5d8e6f0a1b2c3d4e5f")
	if err != nil {
		t.Fatal(err)
	}
	ownUpload := "%MetadataPath%/library/9c/9c3f1d0e7b2a4c5d8e6f0a1b2c3d4e5f/poster-0a1b.jpg"
	replaceImages := RefreshOptions{Metadata: RefreshModeFullRefresh, Image: RefreshModeFullRefresh, ReplaceAllImages: true}
	tests := []struct {
		name      string
		candidate []item.ImageInfo
		stored    []item.ImageInfo
		refresh   RefreshOptions
		want      []item.ImageInfo
	}{
		{"upload beats provider URL",
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: providerURL}},
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: upload}},
			DefaultRefreshOptions(),
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: upload}}},
		{"provider URL beats stored URL",
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: providerURL}},
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: storedURL}},
			DefaultRefreshOptions(),
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: providerURL}}},
		{"library file beats upload",
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: libraryPoster}},
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: upload}},
			DefaultRefreshOptions(),
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: libraryPoster}}},
		{"backdrop moves to the next index",
			[]item.ImageInfo{{Type: item.ImageTypeBackdrop, Path: libraryFanart}},
			[]item.ImageInfo{{Type: item.ImageTypeBackdrop, Path: uploadBackdrop}},
			DefaultRefreshOptions(),
			[]item.ImageInfo{{Type: item.ImageTypeBackdrop, Path: libraryFanart}, {Type: item.ImageTypeBackdrop, Index: 1, Path: uploadBackdrop}}},
		{"missing library file does not carry",
			nil,
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: libraryPoster}},
			DefaultRefreshOptions(),
			nil},
		{"replace all images keeps a type no provider supplied",
			nil,
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: storedURL}, {Type: item.ImageTypeBackdrop, Path: uploadBackdrop}},
			replaceImages,
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: storedURL}, {Type: item.ImageTypeBackdrop, Path: uploadBackdrop}}},
		{"replace all images takes the supplied image",
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: providerURL}},
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: upload}},
			replaceImages,
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: providerURL}}},
		{"replace all metadata and images removes internal metadata images",
			nil,
			[]item.ImageInfo{{Type: item.ImageTypePrimary, Path: ownUpload}, {Type: item.ImageTypeLogo, Path: storedURL}},
			RefreshOptions{Metadata: RefreshModeFullRefresh, Image: RefreshModeFullRefresh, ReplaceAllMetadata: true, ReplaceAllImages: true},
			[]item.ImageInfo{{Type: item.ImageTypeLogo, Path: storedURL}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := item.Item{ImageInfos: test.candidate}
			carryStoredImages(&candidate, item.Item{ID: storedID, ImageInfos: test.stored}, test.refresh, root)
			if !reflect.DeepEqual(candidate.ImageInfos, test.want) {
				t.Fatalf("images = %#v, want %#v", candidate.ImageInfos, test.want)
			}
		})
	}
}

func TestCarryStoredProviderIDsKeepsCandidateValue(t *testing.T) {
	var candidate, previous item.Item
	candidate.ProviderIDs.Set("Imdb", "tt0133093")
	previous.ProviderIDs.Set("Imdb", "tt0000001")
	previous.ProviderIDs.Set("Tmdb", "603")
	carryStoredProviderIDs(&candidate, previous)
	if value, _ := candidate.ProviderIDs.Get("Imdb"); value != "tt0133093" {
		t.Fatalf("Imdb = %q, want the candidate value", value)
	}
	if value, _ := candidate.ProviderIDs.Get("Tmdb"); value != "603" {
		t.Fatalf("Tmdb = %q, want the stored value", value)
	}
}
