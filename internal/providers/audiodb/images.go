// SPDX-License-Identifier: GPL-2.0-only

package audiodb

import (
	"context"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
)

// ImageSupports ports Supports of AudioDbArtistImageProvider (MusicArtist)
// and AudioDbAlbumImageProvider (MusicAlbum). A keyless provider is inert
// and supports no kind (D-11).
func (provider *Provider) ImageSupports(kind item.Kind) bool {
	return provider.api != nil && provider.SupportsKind(kind)
}

// ImageTypes ports GetSupportedImages of the AudioDB image providers.
func (provider *Provider) ImageTypes(kind item.Kind) []item.ImageType {
	if !provider.ImageSupports(kind) {
		return nil
	}
	return provider.SupportedImages(kind)
}

// Images ports GetImages of AudioDbArtistImageProvider, keyed by the
// MusicBrainzArtist ID, and AudioDbAlbumImageProvider, keyed by the
// MusicBrainzReleaseGroup ID. An item without the ID has no images.
func (provider *Provider) Images(ctx context.Context, lookup providers.ImageLookup) ([]providers.RemoteImageInfo, error) {
	if !provider.ImageSupports(lookup.Kind) {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch lookup.Kind {
	case item.KindMusicArtist:
		id := strings.TrimSpace(lookup.ProviderIDs["MusicBrainzArtist"])
		if id == "" {
			return nil, nil
		}
		result, err := provider.api.getArtist(ctx, id)
		if err != nil || result == nil {
			return nil, err
		}
		return remoteImages(
			remoteImage{item.ImageTypePrimary, result.ArtistThumb},
			remoteImage{item.ImageTypeLogo, result.ArtistLogo},
			remoteImage{item.ImageTypeBanner, result.ArtistBanner},
			remoteImage{item.ImageTypeBackdrop, result.ArtistFanart},
			remoteImage{item.ImageTypeBackdrop, result.ArtistFanart2},
			remoteImage{item.ImageTypeBackdrop, result.ArtistFanart3},
		), nil
	case item.KindMusicAlbum:
		id := strings.TrimSpace(lookup.ProviderIDs["MusicBrainzReleaseGroup"])
		if id == "" {
			return nil, nil
		}
		result, err := provider.api.getAlbum(ctx, id)
		if err != nil || result == nil {
			return nil, err
		}
		return remoteImages(
			remoteImage{item.ImageTypePrimary, result.AlbumThumb},
			remoteImage{item.ImageTypeDisc, result.AlbumCDArt},
		), nil
	default:
		return nil, nil
	}
}

type remoteImage struct {
	imageType item.ImageType
	url       string
}

// remoteImages lists the images with a URL in the given order, as the
// upstream GetImages lists each non-blank field.
func remoteImages(images ...remoteImage) []providers.RemoteImageInfo {
	var result []providers.RemoteImageInfo
	for _, image := range images {
		if url := strings.TrimSpace(image.url); url != "" {
			result = append(result, providers.RemoteImageInfo{ProviderName: ProviderName, URL: url, Type: image.imageType})
		}
	}
	return result
}
