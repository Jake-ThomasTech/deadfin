// SPDX-License-Identifier: GPL-2.0-only

package tmdb

import (
	"context"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
)

// refreshBoxSet ports TmdbBoxSetProvider.GetMetadata and
// TmdbBoxSetImageProvider.GetImages. The TMDb collection id comes from the
// Tmdb provider id or, without one, from the first /search/collection
// result for the cleaned name. The library sets the Tmdb id from the
// members' TmdbCollection id before this runs (divergence D-116). The
// collection supplies Name, Overview, the Tmdb id, and the first poster and
// backdrop.
func (provider *Provider) refreshBoxSet(ctx context.Context, target *item.Item, language, country string, replaceImages bool) (bool, error) {
	tmdbID := boxSetTMDbID(target.ProviderIDs)
	if tmdbID <= 0 && target.Name != nil {
		parsed, _ := parseName(*target.Name, provider.naming)
		name := strings.TrimSpace(cleanName(parsed))
		if name != "" {
			results, err := provider.api.searchCollection(ctx, name, language)
			if err != nil {
				return false, err
			}
			if len(results) > 0 {
				tmdbID = results[0].ID
			}
		}
	}
	if tmdbID <= 0 {
		return false, nil
	}
	result, err := provider.api.getCollection(ctx, tmdbID, language, imageLanguagesParam(language, country))
	if err != nil {
		return false, err
	}
	if result == nil {
		return false, nil
	}
	setString(&target.Name, result.Name)
	setString(&target.Overview, result.Overview)
	target.ProviderIDs.Set("Tmdb", strconv.Itoa(result.ID))
	provider.applyImages(ctx, target, result.PosterPath, result.Images, replaceImages)
	return true, nil
}

func boxSetTMDbID(ids item.ProviderIDs) int {
	value, ok := ids.Get("Tmdb")
	if !ok {
		return 0
	}
	id, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return id
}

// collectionImages ports TmdbBoxSetImageProvider.GetImages: the posters as
// Primary and the backdrops as Backdrop, or Thumb for a backdrop with a
// language (ConvertToRemoteImageInfo).
func collectionImages(result *collection, base, size string) []providers.RemoteImageInfo {
	var out []providers.RemoteImageInfo
	add := func(imgType item.ImageType, data imageData) {
		out = append(out, providers.RemoteImageInfo{
			ProviderName:    ProviderName,
			URL:             imageURL(base, size, data.FilePath),
			Height:          data.Height,
			Width:           data.Width,
			CommunityRating: data.VoteAverage,
			VoteCount:       data.VoteCount,
			Language:        adjustImageLanguage(data.Iso6391, ""),
			Type:            imgType,
			RatingType:      "Score",
		})
	}
	if result.Images == nil {
		return nil
	}
	for _, poster := range result.Images.Posters {
		add(item.ImageTypePrimary, poster)
	}
	for _, backdrop := range result.Images.Backdrops {
		imgType := item.ImageTypeBackdrop
		if adjustImageLanguage(backdrop.Iso6391, "") != "" {
			imgType = item.ImageTypeThumb
		}
		add(imgType, backdrop)
	}
	return out
}
