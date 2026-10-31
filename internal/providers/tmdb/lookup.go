// SPDX-License-Identifier: GPL-2.0-only

package tmdb

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
)

// LookupProvider implements the remote-search and remote-image capabilities
// for TMDb Movie and Series. It wraps the same apiClient the refresh Provider
// uses, so it shares the cassette-backed transport and is inert without an
// API key (D-11).
type LookupProvider struct {
	api    *apiClient
	config Config
	logger *slog.Logger
}

// NewLookupProvider builds the TMDb lookup provider from the same options as the
// refresh provider. A nil transport or empty API key yields an inert provider:
// SearchSupports and ImageSupports return false so the provider drops out of
// listings (D-11).
func NewLookupProvider(options Options) *LookupProvider {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var api *apiClient
	if options.HTTP != nil && options.Config.APIKey != "" {
		api = newAPIClient(options.HTTP, options.Config.APIKey, logger)
	}
	return &LookupProvider{api: api, config: options.Config, logger: logger}
}

func (*LookupProvider) Name() string { return ProviderName }

// SearchSupports reports the kinds TMDb can search (Movie, Series). Inert
// providers return false for every kind.
func (provider *LookupProvider) SearchSupports(kind item.Kind) bool {
	if provider.api == nil {
		return false
	}
	return kind == item.KindMovie || kind == item.KindSeries
}

// ImageSupports reports the kinds TMDb can list images for (Movie, Series).
func (provider *LookupProvider) ImageSupports(kind item.Kind) bool {
	if provider.api == nil {
		return false
	}
	return kind == item.KindMovie || kind == item.KindSeries
}

// ImageTypes returns the image types TMDb supplies for the kind, mirroring
// TmdbMovieImageProvider.GetSupportedImages and TmdbSeriesImageProvider.
func (provider *LookupProvider) ImageTypes(kind item.Kind) []item.ImageType {
	if !provider.ImageSupports(kind) {
		return nil
	}
	return []item.ImageType{
		item.ImageTypePrimary,
		item.ImageTypeBackdrop,
		item.ImageTypeLogo,
		item.ImageTypeThumb,
	}
}

// Search runs a TMDb remote search for the given lookup, returning results in
// provider order. It reuses the existing search ladder: provider IDs first
// (TMDb, IMDb/TVDB find), then name+year search (D-13 query cleaning).
func (provider *LookupProvider) Search(ctx context.Context, lookup providers.Lookup) ([]providers.RemoteSearchResult, error) {
	if provider.api == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	language := normalizeLanguage(strings.TrimSpace(lookup.Language), lookup.Country)
	imageBase := provider.api.imageBaseURL(ctx)

	switch lookup.Kind {
	case item.KindMovie:
		return provider.searchMovie(ctx, lookup, language, imageBase)
	case item.KindSeries:
		return provider.searchSeries(ctx, lookup, language, imageBase)
	default:
		return nil, nil
	}
}
func (provider *LookupProvider) searchMovie(ctx context.Context, lookup providers.Lookup, language, imageBase string) ([]providers.RemoteSearchResult, error) {
	// If we have a TMDb id, fetch directly and return a single result.
	if tmdbID := providerIDInt(lookup.ProviderIDs, "Tmdb"); tmdbID > 0 {
		result, err := provider.api.getMovie(ctx, tmdbID, language, imageLanguagesParam(language, lookup.Country))
		if err != nil {
			return nil, err
		}
		if result != nil {
			return []providers.RemoteSearchResult{movieToSearchResult(result, language, imageBase)}, nil
		}
	}

	var results []providers.RemoteSearchResult
	// Try IMDb find.
	if imdb := providerIDString(lookup.ProviderIDs, "Imdb"); imdb != "" {
		found, err := provider.api.findByExternalID(ctx, imdb, "imdb_id", language)
		if err != nil {
			return nil, err
		}
		if found != nil {
			for _, m := range found.MovieResults {
				results = append(results, searchMovieToResult(m, language, imageBase))
			}
		}
	}
	if len(results) > 0 {
		return results, nil
	}

	// Name+year search.
	name := strings.TrimSpace(lookup.Name)
	if name == "" {
		return nil, nil
	}
	query := searchQuery(name)
	if query == "" {
		return nil, nil
	}
	year := 0
	if lookup.Year != nil {
		year = *lookup.Year
	}
	movies, err := provider.api.searchMovie(ctx, query, year, language, provider.config.IncludeAdult)
	if err != nil {
		return nil, err
	}
	for _, m := range movies {
		results = append(results, searchMovieToResult(m, language, imageBase))
	}
	return results, nil
}

func (provider *LookupProvider) searchSeries(ctx context.Context, lookup providers.Lookup, language, imageBase string) ([]providers.RemoteSearchResult, error) {
	// If we have a TMDb id, fetch directly and return a single result.
	if tmdbID := providerIDInt(lookup.ProviderIDs, "Tmdb"); tmdbID > 0 {
		result, err := provider.api.getSeries(ctx, tmdbID, language, imageLanguagesParam(language, lookup.Country))
		if err != nil {
			return nil, err
		}
		if result != nil {
			return []providers.RemoteSearchResult{seriesToSearchResult(result, language, imageBase)}, nil
		}
	}

	var results []providers.RemoteSearchResult
	// Try IMDb and TVDB find.
	for _, external := range []struct{ key, source string }{
		{"Imdb", "imdb_id"},
		{"Tvdb", "tvdb_id"},
	} {
		value := providerIDString(lookup.ProviderIDs, external.key)
		if value == "" {
			continue
		}
		found, err := provider.api.findSeriesByExternalID(ctx, value, external.source, language)
		if err != nil {
			return nil, err
		}
		for _, s := range found {
			results = append(results, searchTvToResult(s, language, imageBase))
		}
		if len(results) > 0 {
			return results, nil
		}
	}

	// Name+year search.
	name := strings.TrimSpace(lookup.Name)
	if name == "" {
		return nil, nil
	}
	query := searchQuery(name)
	if query == "" {
		return nil, nil
	}
	year := 0
	if lookup.Year != nil {
		year = *lookup.Year
	}
	series, err := provider.api.searchSeries(ctx, query, year, language, provider.config.IncludeAdult)
	if err != nil {
		return nil, err
	}
	for _, s := range series {
		results = append(results, searchTvToResult(s, language, imageBase))
	}
	return results, nil
}

// Images lists TMDb images for a movie or series, mirroring
// TmdbMovieImageProvider.GetImages and TmdbSeriesImageProvider.GetImages. It
// fetches the detail response (which includes the images collection) and maps
// posters, backdrops, and logos. Language-tagged backdrops are mapped to Thumb
// as in ConvertToRemoteImageInfo (provider.go:237-269). Like upstream's
// GetMovieAsync(id, null, null, null), the request carries neither language nor
// include_image_language: TMDb filters the images collection to the request
// language when language is set, which drops the untagged backdrops. Without
// both params TMDb returns images in all languages; the identify service's
// FilterImagesByLanguage and SortRemoteImages apply the language filter and
// ordering post-fetch, honoring includeAllLanguages.
func (provider *LookupProvider) Images(ctx context.Context, lookup providers.ImageLookup) ([]providers.RemoteImageInfo, error) {
	if provider.api == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base := provider.api.imageBaseURL(ctx)
	size := provider.config.imageSizeOrOriginal()

	switch lookup.Kind {
	case item.KindMovie:
		tmdbID := providerIDInt(lookup.ProviderIDs, "Tmdb")
		if tmdbID == 0 {
			return nil, nil
		}
		result, err := provider.api.getMovie(ctx, tmdbID, "", "")
		if err != nil || result == nil {
			return nil, err
		}
		return movieImages(result, base, size), nil
	case item.KindSeries:
		tmdbID := providerIDInt(lookup.ProviderIDs, "Tmdb")
		if tmdbID == 0 {
			return nil, nil
		}
		result, err := provider.api.getSeries(ctx, tmdbID, "", "")
		if err != nil || result == nil {
			return nil, err
		}
		return seriesImages(result, base, size), nil
	default:
		return nil, nil
	}
}

func movieImages(result *movie, base, size string) []providers.RemoteImageInfo {
	var images []providers.RemoteImageInfo
	add := func(imgType item.ImageType, data imageData) {
		images = append(images, providers.RemoteImageInfo{
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
	if result.Images != nil {
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
		for _, logo := range result.Images.Logos {
			add(item.ImageTypeLogo, logo)
		}
	}
	// Fallback: poster_path when no image collection.
	if len(images) == 0 && result.PosterPath != "" {
		images = append(images, providers.RemoteImageInfo{
			ProviderName: ProviderName,
			URL:          imageURL(base, size, result.PosterPath),
			Type:         item.ImageTypePrimary,
			RatingType:   "Score",
		})
	}
	return images
}

func seriesImages(result *tvShow, base, size string) []providers.RemoteImageInfo {
	var images []providers.RemoteImageInfo
	add := func(imgType item.ImageType, data imageData) {
		images = append(images, providers.RemoteImageInfo{
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
	if result.Images != nil {
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
		for _, logo := range result.Images.Logos {
			add(item.ImageTypeLogo, logo)
		}
	}
	if len(images) == 0 && result.PosterPath != "" {
		images = append(images, providers.RemoteImageInfo{
			ProviderName: ProviderName,
			URL:          imageURL(base, size, result.PosterPath),
			Type:         item.ImageTypePrimary,
			RatingType:   "Score",
		})
	}
	return images
}

// --- search result mapping ---

func movieToSearchResult(result *movie, language, imageBase string) providers.RemoteSearchResult {
	r := providers.RemoteSearchResult{
		Name:        stringPtrOrNil(firstNonEmpty(result.Title, result.OriginalTitle)),
		ProviderIDs: map[string]string{"Tmdb": strconv.Itoa(result.ID)},
		Overview:    stringPtrOrNil(strings.ReplaceAll(result.Overview, "\n\n", "\n")),
		ImageURL:    imageURL(imageBase, "w185", result.PosterPath),
	}
	if imdb := strings.TrimSpace(result.ImdbID); imdb != "" {
		r.ProviderIDs["Imdb"] = imdb
	}
	if premiere, ok := parseTMDbDate(result.ReleaseDate); ok {
		formatted := premiere.Format("2006-01-02T15:04:05.0000000Z")
		r.PremiereDate = &formatted
		year := premiere.Year()
		r.ProductionYear = &year
	}
	return r
}

func searchMovieToResult(m searchMovie, language, imageBase string) providers.RemoteSearchResult {
	r := providers.RemoteSearchResult{
		Name:        stringPtrOrNil(m.Title),
		ProviderIDs: map[string]string{"Tmdb": strconv.Itoa(m.ID)},
		Overview:    stringPtrOrNil(m.Overview),
		ImageURL:    imageURL(imageBase, "w185", m.PosterPath),
	}
	if premiere, ok := parseTMDbDate(m.ReleaseDate); ok {
		formatted := premiere.Format("2006-01-02T15:04:05.0000000Z")
		r.PremiereDate = &formatted
		year := premiere.Year()
		r.ProductionYear = &year
	}
	return r
}

func seriesToSearchResult(result *tvShow, language, imageBase string) providers.RemoteSearchResult {
	r := providers.RemoteSearchResult{
		Name:        stringPtrOrNil(result.Name),
		ProviderIDs: map[string]string{"Tmdb": strconv.Itoa(result.ID)},
		Overview:    stringPtrOrNil(result.Overview),
		ImageURL:    imageURL(imageBase, "w185", result.PosterPath),
	}
	if result.ExternalIDs != nil {
		if imdb := strings.TrimSpace(result.ExternalIDs.ImdbID); imdb != "" {
			r.ProviderIDs["Imdb"] = imdb
		}
		if result.ExternalIDs.TvdbID > 0 {
			r.ProviderIDs["Tvdb"] = strconv.Itoa(result.ExternalIDs.TvdbID)
		}
	}
	if premiere, ok := parseTMDbDate(result.FirstAirDate); ok {
		formatted := premiere.Format("2006-01-02T15:04:05.0000000Z")
		r.PremiereDate = &formatted
		year := premiere.Year()
		r.ProductionYear = &year
	}
	return r
}

func searchTvToResult(s searchTv, language, imageBase string) providers.RemoteSearchResult {
	r := providers.RemoteSearchResult{
		Name:        stringPtrOrNil(s.Name),
		ProviderIDs: map[string]string{"Tmdb": strconv.Itoa(s.ID)},
		Overview:    stringPtrOrNil(s.Overview),
		ImageURL:    imageURL(imageBase, "w185", s.PosterPath),
	}
	if premiere, ok := parseTMDbDate(s.FirstAirDate); ok {
		formatted := premiere.Format("2006-01-02T15:04:05.0000000Z")
		r.PremiereDate = &formatted
		year := premiere.Year()
		r.ProductionYear = &year
	}
	return r
}

func stringPtrOrNil(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func providerIDInt(ids map[string]string, name string) int {
	if ids == nil {
		return 0
	}
	for key, value := range ids {
		if strings.EqualFold(key, name) {
			id, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil && id > 0 {
				return id
			}
			break
		}
	}
	return 0
}

func providerIDString(ids map[string]string, name string) string {
	if ids == nil {
		return ""
	}
	for key, value := range ids {
		if strings.EqualFold(key, name) {
			value = strings.TrimSpace(value)
			if value != "" {
				return value
			}
			break
		}
	}
	return ""
}

// Compile-time interface checks.
var (
	_ providers.Searcher    = (*LookupProvider)(nil)
	_ providers.ImageLister = (*LookupProvider)(nil)
)

// remote is referenced to avoid an unused import when building with no remote
// callers (the LookupProvider uses apiClient which uses remote.Client).
var _ = (*remote.Client)(nil)
