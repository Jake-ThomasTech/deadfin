// SPDX-License-Identifier: GPL-2.0-only

package tmdb

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/remote"
)

// Provider fetches TMDb metadata and artwork during the remote-metadata stage
// of the refresh pipeline. It is constructed in the composition root with the
// shared transport and the resolved provider configuration; without an API key
// it is inert (D-11).
type Provider struct {
	api    *apiClient
	config Config
	logger *slog.Logger
	naming naming.NamingOptions
}

// Options constructs a Provider.
type Options struct {
	HTTP   *remote.Client
	Config Config
	// Logger records credential-safe provider observations. Nil selects slog.Default.
	Logger *slog.Logger
}

// NewProvider builds the TMDb refresh provider. A nil transport or empty API key
// yields an inert provider that enriches nothing.
func NewProvider(options Options) *Provider {
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var api *apiClient
	if options.HTTP != nil && options.Config.APIKey != "" {
		api = newAPIClient(options.HTTP, options.Config.APIKey, logger)
	}
	return &Provider{api: api, config: options.Config, logger: logger, naming: naming.DefaultOptions()}
}

// Name is the pipeline identity matched against options.xml MetadataFetchers.
func (*Provider) Name() string { return ProviderName }

// Stage places the provider in the remote-metadata phase, so it runs only when
// the library enables internet providers and after local/NFO metadata.
func (*Provider) Stage() providers.ProviderStage { return providers.ProviderStageRemoteMetadata }

// SupportsKind reports the item kinds that the provider enriches.
func (provider *Provider) SupportsKind(kind item.Kind) bool { return provider.supports(kind) }

// SupportedImages ports GetSupportedImages of TmdbMovieImageProvider,
// TmdbSeriesImageProvider, TmdbSeasonImageProvider,
// TmdbEpisodeImageProvider, and TmdbBoxSetImageProvider.
func (*Provider) SupportedImages(kind item.Kind) []item.ImageType {
	switch kind {
	case item.KindMovie, item.KindSeries:
		return []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop, item.ImageTypeLogo, item.ImageTypeThumb}
	case item.KindSeason, item.KindEpisode:
		return []item.ImageType{item.ImageTypePrimary}
	case item.KindBoxSet:
		return []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop, item.ImageTypeThumb}
	default:
		return nil
	}
}

// supports reports the item kinds the ported provider enriches. Season and
// Episode read the parent series from RefreshContext.Ancestors. Person is not
// ported.
func (*Provider) supports(kind item.Kind) bool {
	switch kind {
	case item.KindMovie, item.KindSeries, item.KindSeason, item.KindEpisode, item.KindBoxSet:
		return true
	default:
		return false
	}
}

// Refresh enriches target from TMDb. It returns handled=false (never an error)
// when the provider is inert, the kind is unsupported, or no match is found, so
// a missing key or an unmatched title never fails a library scan.
func (provider *Provider) Refresh(ctx context.Context, refresh library.RefreshContext, target *item.Item) (bool, error) {
	if target == nil {
		return false, errors.New("tmdb: item is required")
	}
	if provider.logger.Enabled(ctx, slog.LevelDebug) {
		provider.logger.LogAttrs(ctx, slog.LevelDebug, "tmdb refresh",
			slog.String("item", target.ID.String()),
			slog.String("kind", target.Kind.String()),
			slog.Bool("inert", provider.api == nil),
			slog.Bool("supported", provider.supports(target.Kind)),
		)
	}
	if provider.api == nil || !provider.supports(target.Kind) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	language := normalizeLanguage(strings.TrimSpace(refresh.Options.PreferredMetadataLanguage), refresh.Options.MetadataCountryCode)
	country := strings.TrimSpace(refresh.Options.MetadataCountryCode)
	// Mirror ImageRefreshOptions.IsReplacingImage: an existing image of a type is
	// overwritten only under a full image refresh with replace-all set. Otherwise
	// remote artwork fills a missing type and leaves existing artwork in place.
	replaceImages := refresh.ReplaceAllImages && refresh.Image == library.RefreshModeFullRefresh

	switch target.Kind {
	case item.KindMovie:
		return provider.refreshMovie(ctx, target, language, country, refresh.LibraryRoot, replaceImages)
	case item.KindSeries:
		return provider.refreshSeries(ctx, target, language, country, refresh.LibraryRoot, replaceImages)
	case item.KindSeason:
		return provider.refreshSeason(ctx, refresh, target, language, country, replaceImages)
	case item.KindEpisode:
		return provider.refreshEpisode(ctx, refresh, target, language, country, replaceImages)
	case item.KindBoxSet:
		return provider.refreshBoxSet(ctx, target, language, country, replaceImages)
	default:
		return false, nil
	}
}

// refreshMovie ports TmdbMovieProvider.GetMetadata: resolve a TMDb id (from
// provider ids, then an IMDb find, then a name+year search), fetch the movie,
// and map it onto target.
func (provider *Provider) refreshMovie(ctx context.Context, target *item.Item, language, country, libraryRoot string, replaceImages bool) (bool, error) {
	tmdbID, err := provider.resolveMovieID(ctx, target, language, libraryRoot)
	if err != nil {
		return false, err
	}
	if tmdbID == 0 {
		return false, nil
	}
	result, err := provider.api.getMovie(ctx, tmdbID, language, imageLanguagesParam(language, country))
	if err != nil {
		return false, err
	}
	if result == nil {
		return false, nil
	}
	provider.applyMovie(ctx, target, result, language, country, replaceImages)
	return true, nil
}

// resolveMovieID mirrors the id-resolution ladder in GetMetadata.
func (provider *Provider) resolveMovieID(ctx context.Context, target *item.Item, language, libraryRoot string) (int, error) {
	if value, ok := target.ProviderIDs.Get("Tmdb"); ok {
		if id, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && id > 0 {
			return id, nil
		}
	}
	if value, ok := target.ProviderIDs.Get("Imdb"); ok && strings.TrimSpace(value) != "" {
		found, err := provider.api.findByExternalID(ctx, strings.TrimSpace(value), "imdb_id", language)
		if err != nil {
			return 0, err
		}
		if found != nil && len(found.MovieResults) > 0 {
			return found.MovieResults[0].ID, nil
		}
	}

	primary, fallback, year := provider.searchTerms(target, libraryRoot)
	if primary == "" && fallback == "" {
		return 0, nil
	}
	provider.logger.LogAttrs(ctx, slog.LevelDebug, "tmdb search",
		slog.String("item", target.ID.String()),
		slog.String("kind", target.Kind.String()),
		slog.String("query", primary),
		slog.String("fallback", fallback),
		slog.Int("year", year))
	return firstMatchID(ctx, primary, fallback, year, func(ctx context.Context, query string, y int) (int, error) {
		results, err := provider.api.searchMovie(ctx, query, y, language, provider.config.IncludeAdult)
		if err != nil {
			return 0, err
		}
		if len(results) == 0 {
			return 0, nil
		}
		return results[0].ID, nil
	})
}

// applyMovie maps a TMDb movie onto target, following the field selection in
// TmdbMovieProvider.GetMetadata.
func (provider *Provider) applyMovie(ctx context.Context, target *item.Item, result *movie, language, country string, replaceImages bool) {
	setString(&target.Name, firstNonEmpty(result.Title, result.OriginalTitle))
	setString(&target.OriginalTitle, result.OriginalTitle)
	setString(&target.Overview, strings.ReplaceAll(result.Overview, "\n\n", "\n"))
	setString(&target.Tagline, result.Tagline)
	setString(&target.OriginalLanguage, result.OriginalLanguage)

	if locations := names(result.ProductionCountries); len(locations) > 0 {
		target.Values.ProductionLocations = locations
	}

	target.ProviderIDs.Set("Tmdb", strconv.Itoa(result.ID))
	if imdb := strings.TrimSpace(result.ImdbID); imdb != "" {
		target.ProviderIDs.Set("Imdb", imdb)
	}
	if result.BelongsToCollection != nil {
		target.ProviderIDs.Set("TmdbCollection", strconv.Itoa(result.BelongsToCollection.ID))
		collectionName := result.BelongsToCollection.Name
		if target.Extra.Movie == nil {
			target.Extra.Movie = &item.MovieExtra{}
		}
		target.Extra.Movie.TmdbCollectionName = &collectionName
	}

	rating := float32(result.VoteAverage)
	target.CommunityRating = &rating

	if official := officialRating(result.ReleaseDates, country); official != "" {
		setString(&target.OfficialRating, official)
	}

	if premiere, ok := parseTMDbDate(result.ReleaseDate); ok {
		target.PremiereDate = &premiere
		year := premiere.Year()
		target.ProductionYear = &year
	}

	if studios := namesFrom(result.ProductionCompanies); len(studios) > 0 {
		target.Values.Studios = studios
	}
	if genres := trimmedNames(result.Genres); len(genres) > 0 {
		target.Values.Genres = genres
	}
	if result.Keywords != nil && !provider.config.ExcludeTagsMovies {
		if tags := trimmedNames(result.Keywords.Keywords); len(tags) > 0 {
			target.Values.Tags = tags
		}
	}

	if people := provider.moviePeople(ctx, result.Credits); len(people) > 0 {
		target.People = people
	}

	if trailers := trailers(result.Videos); len(trailers) > 0 {
		target.Extra.RemoteTrailers = trailers
	}

	provider.applyImages(ctx, target, result.PosterPath, result.Images, replaceImages)
}

// moviePeople selects cast and wanted crew for a movie via the shared mapper.
func (provider *Provider) moviePeople(ctx context.Context, credits *credits) []item.PersonCredit {
	if credits == nil {
		return nil
	}
	return mapCastCrew(provider.config, provider.profileURL(ctx), credits.Cast, credits.Crew)
}

// applyImages adds remote image URLs for types local artwork did not supply.
// Backdrops carrying a language (that is, containing text) become Thumb images,
// matching ConvertToRemoteImageInfo.
func (provider *Provider) applyImages(ctx context.Context, target *item.Item, posterPath string, artwork *images, replace bool) {
	base := provider.api.imageBaseURL(ctx)
	size := provider.config.imageSizeOrOriginal()

	add := func(kind item.ImageType, data imageData) {
		if data.FilePath == "" {
			return
		}
		putRemoteImage(target, kind, imageURL(base, size, data.FilePath), data.Width, data.Height, replace)
	}

	// poster_path carries no dimensions; the image collection does.
	primary := imageData{FilePath: posterPath}
	if artwork != nil && len(artwork.Posters) > 0 {
		primary = artwork.Posters[0]
	}
	add(item.ImageTypePrimary, primary)

	if artwork != nil {
		// Only the first backdrop of each type attaches, so a replacing
		// refresh keeps TMDb's top-ranked one instead of the last.
		added := map[item.ImageType]bool{}
		for _, backdrop := range artwork.Backdrops {
			kind := item.ImageTypeBackdrop
			if adjustImageLanguage(backdrop.Iso6391, "") != "" {
				kind = item.ImageTypeThumb
			}
			if added[kind] || backdrop.FilePath == "" {
				continue
			}
			added[kind] = true
			add(kind, backdrop)
		}
		if len(artwork.Logos) > 0 {
			add(item.ImageTypeLogo, artwork.Logos[0])
		}
	}
}

// --- small mapping helpers ---

func setString(target **string, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	clone := value
	*target = &clone
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func names(countries []productionCountry) []string {
	out := make([]string, 0, len(countries))
	for _, country := range countries {
		if name := strings.TrimSpace(country.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func namesFrom(values []namedValue) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if name := strings.TrimSpace(value.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func trimmedNames(values []namedValue) []string { return namesFrom(values) }

func filterCast(cast []castMember) []castMember {
	out := cast[:0]
	for _, actor := range cast {
		if strings.TrimSpace(actor.ProfilePath) != "" {
			out = append(out, actor)
		}
	}
	return out
}

func trailers(videos *videos) []item.MediaURL {
	if videos == nil {
		return nil
	}
	ordered := append([]video(nil), videos.Results...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return strings.EqualFold(ordered[i].Type, "trailer") && !strings.EqualFold(ordered[j].Type, "trailer")
	})
	out := make([]item.MediaURL, 0, len(ordered))
	for _, entry := range ordered {
		if !isTrailerVideo(entry.Site, entry.Type) {
			continue
		}
		url := "https://www.youtube.com/watch?v=" + entry.Key
		name := entry.Name
		out = append(out, item.MediaURL{URL: &url, Name: &name})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// officialRating selects the certification for the requested country, falling
// back to the US certification (BuildParentalRating handles prefixing).
func officialRating(dates *releaseDates, country string) string {
	if dates == nil {
		return ""
	}
	countryCert := func(iso string) string {
		for _, entry := range dates.Results {
			if !strings.EqualFold(entry.Iso31661, iso) {
				continue
			}
			for _, release := range entry.ReleaseDates {
				if cert := strings.TrimSpace(release.Certification); cert != "" {
					return cert
				}
			}
		}
		return ""
	}
	if country != "" {
		if cert := countryCert(country); cert != "" {
			return buildParentalRating(country, cert)
		}
	}
	if cert := countryCert("US"); cert != "" {
		return cert
	}
	return ""
}

// putRemoteImage records a remote image URL and its dimensions for kind
// (zero when unknown). It fills a missing type and, when replace is set
// (upstream ImageRefreshOptions.IsReplacingImage: ImageRefreshMode==FullRefresh
// with ReplaceAllImages), overwrites the existing entry of that type instead of
// leaving local/prior artwork untouched.
func putRemoteImage(target *item.Item, kind item.ImageType, url string, width, height int, replace bool) {
	if url == "" {
		return
	}
	for index := range target.ImageInfos {
		if target.ImageInfos[index].Type != kind {
			continue
		}
		if replace {
			target.ImageInfos[index].Path = url
			target.ImageInfos[index].Width = width
			target.ImageInfos[index].Height = height
		}
		return
	}
	target.ImageInfos = append(target.ImageInfos, item.ImageInfo{Type: kind, Path: url, Width: width, Height: height})
}

// parseTMDbDate parses TMDb's yyyy-mm-dd date in UTC.
func parseTMDbDate(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
