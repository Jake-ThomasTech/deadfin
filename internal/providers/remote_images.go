// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
)

// refreshRemoteImages ports the IRemoteImageProvider pass of
// ItemImageProvider.RefreshImages (RefreshFromProvider). The enabled image
// fetchers run in ImageFetcherOrder when the scanner set RunRemoteImages.
// Each fetcher fills an image type that is enabled (ImageLimit above zero)
// and that the item and the stored item do not have; a refresh that replaces
// all images does not count the stored images. The image is stored as its
// remote URL, as the metadata providers store theirs.
//
// Deadfin's remote metadata providers set their images in the remote
// metadata stage. A fetcher whose metadata provider of the same name handled
// the item in this refresh is therefore not asked again. A fetcher failure
// is logged and counted, and the next fetcher runs.
func (r *Registry) refreshRemoteImages(ctx context.Context, refresh library.RefreshContext, target *item.Item, handled map[string]bool) (int, error) {
	if len(r.imageListers) == 0 || !refresh.RunRemoteImages || refresh.Image <= library.RefreshModeValidationOnly {
		return 0, nil
	}
	// ProviderManager.CanRefreshImages: a locked item refreshes remote
	// images only on a FullRefresh.
	if target.IsLocked && refresh.Image != library.RefreshModeFullRefresh {
		return 0, nil
	}
	replacing := refresh.ReplaceAllImages && refresh.Image == library.RefreshModeFullRefresh
	missing := func(imageType item.ImageType) bool {
		if refresh.Options.ImageLimit(target.Kind, imageType) <= 0 || hasImageType(target.ImageInfos, imageType) {
			return false
		}
		return replacing || !hasImageType(refresh.StoredImages, imageType)
	}
	failures := 0
	for _, lister := range r.enabledImageListers(refresh.Options, target.Kind) {
		if err := ctx.Err(); err != nil {
			return failures, err
		}
		if handled[strings.ToLower(lister.Name())] {
			continue
		}
		var types []item.ImageType
		for _, imageType := range lister.ImageTypes(target.Kind) {
			if missing(imageType) {
				types = append(types, imageType)
			}
		}
		if len(types) == 0 {
			continue
		}
		language := strings.TrimSpace(refresh.Options.PreferredMetadataLanguage)
		images, err := lister.Images(ctx, ImageLookup{Kind: target.Kind, Language: language, ProviderIDs: providerIDMap(target.ProviderIDs)})
		if err != nil {
			if ctx.Err() != nil {
				return failures, ctx.Err()
			}
			failures++
			r.log().LogAttrs(ctx, slog.LevelWarn, "remote image provider failed",
				slog.String("provider", lister.Name()),
				slog.String("item", target.ID.String()),
				slog.String("kind", target.Kind.String()),
				slog.String("error", err.Error()))
			continue
		}
		added := 0
		for _, imageType := range types {
			limit := 1
			if imageType == item.ImageTypeBackdrop {
				limit = refresh.Options.ImageLimit(target.Kind, imageType)
			}
			for index, url := range remoteImageURLs(images, imageType, language, limit) {
				target.ImageInfos = append(target.ImageInfos, item.ImageInfo{Type: imageType, Index: index, Path: url})
				added++
			}
		}
		r.log().LogAttrs(ctx, slog.LevelDebug, "remote image provider ran",
			slog.String("provider", lister.Name()),
			slog.String("item", target.ID.String()),
			slog.String("kind", target.Kind.String()),
			slog.Int("images", added))
	}
	return failures, nil
}

// enabledImageListers ports ProviderManager.GetImageProvidersInternal for
// the remote image providers: the listers that support kind and that the
// library's ImageFetchers enable (or the server does not disable), in
// ImageFetcherOrder, then in default order.
func (r *Registry) enabledImageListers(options library.LibraryOptions, kind item.Kind) []ImageLister {
	typeOptions := options.TypeOptionsFor(kind)
	var order []string
	if typeOptions != nil {
		order = typeOptions.ImageFetcherOrder
	}
	serverDisabled := r.serverDisabledImageFetchers(kind)
	var enabled []ImageLister
	for _, lister := range r.imageListers {
		if lister != nil && lister.ImageSupports(kind) && imageFetcherEnabled(typeOptions, serverDisabled, lister.Name()) {
			enabled = append(enabled, lister)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		return configuredOrder(order, enabled[i].Name()) < configuredOrder(order, enabled[j].Name())
	})
	return enabled
}

// remoteImageURLs returns up to limit distinct URLs of imageType. Images in
// language come first, then images without a language, then the others, as
// the remote image query orders them for the preferred language.
func remoteImageURLs(images []RemoteImageInfo, imageType item.ImageType, language string, limit int) []string {
	rank := func(image RemoteImageInfo) int {
		switch {
		case language != "" && strings.EqualFold(image.Language, language):
			return 0
		case image.Language == "":
			return 1
		default:
			return 2
		}
	}
	var candidates []RemoteImageInfo
	for _, image := range images {
		if image.Type == imageType && strings.TrimSpace(image.URL) != "" {
			candidates = append(candidates, image)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return rank(candidates[i]) < rank(candidates[j]) })
	var urls []string
	for _, image := range candidates {
		if len(urls) >= limit {
			break
		}
		url := strings.TrimSpace(image.URL)
		if !containsFold(urls, url) {
			urls = append(urls, url)
		}
	}
	return urls
}

// providerIDMap copies ids into the map form of ImageLookup.
func providerIDMap(ids item.ProviderIDs) map[string]string {
	out := make(map[string]string)
	for _, id := range ids.Snapshot() {
		out[id.Name] = id.Value
	}
	return out
}
