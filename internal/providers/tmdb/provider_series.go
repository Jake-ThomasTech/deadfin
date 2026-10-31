// SPDX-License-Identifier: GPL-2.0-only

package tmdb

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
)

// refreshSeries ports TmdbSeriesProvider.GetMetadata: resolve a series TMDb id
// (provider ids, then an IMDb/TVDB find, then a name+year search) and map it.
func (provider *Provider) refreshSeries(ctx context.Context, target *item.Item, language, country, libraryRoot string, replaceImages bool) (bool, error) {
	tmdbID, err := provider.resolveSeriesID(ctx, target, language, libraryRoot)
	if err != nil {
		return false, err
	}
	if tmdbID == 0 {
		return false, nil
	}
	result, err := provider.api.getSeries(ctx, tmdbID, language, imageLanguagesParam(language, country))
	if err != nil {
		return false, err
	}
	if result == nil {
		return false, nil
	}
	provider.applySeries(ctx, target, result, country, replaceImages)
	return true, nil
}

// resolveSeriesID mirrors the id-resolution ladder in the series GetMetadata.
func (provider *Provider) resolveSeriesID(ctx context.Context, target *item.Item, language, libraryRoot string) (int, error) {
	if value, ok := target.ProviderIDs.Get("Tmdb"); ok {
		if id, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && id > 0 {
			return id, nil
		}
	}
	for _, external := range []struct{ key, source string }{
		{"Imdb", "imdb_id"},
		{"Tvdb", "tvdb_id"},
	} {
		value, ok := target.ProviderIDs.Get(external.key)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		results, err := provider.api.findSeriesByExternalID(ctx, strings.TrimSpace(value), external.source, language)
		if err != nil {
			return 0, err
		}
		if len(results) > 0 {
			return results[0].ID, nil
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
		results, err := provider.api.searchSeries(ctx, query, y, language, provider.config.IncludeAdult)
		if err != nil {
			return 0, err
		}
		if len(results) == 0 {
			return 0, nil
		}
		return results[0].ID, nil
	})
}

// applySeries maps a TMDb series onto target per MapTvShowToSeries + GetPersons.
func (provider *Provider) applySeries(ctx context.Context, target *item.Item, result *tvShow, country string, replaceImages bool) {
	setString(&target.Name, result.Name)
	setString(&target.OriginalTitle, result.OriginalName)
	setString(&target.Overview, result.Overview)
	setString(&target.OriginalLanguage, result.OriginalLanguage)

	target.ProviderIDs.Set("Tmdb", strconv.Itoa(result.ID))
	if result.ExternalIDs != nil {
		if imdb := strings.TrimSpace(result.ExternalIDs.ImdbID); imdb != "" {
			target.ProviderIDs.Set("Imdb", imdb)
		}
		if result.ExternalIDs.TvdbID > 0 {
			target.ProviderIDs.Set("Tvdb", strconv.Itoa(result.ExternalIDs.TvdbID))
		}
		if result.ExternalIDs.TvrageID > 0 {
			target.ProviderIDs.Set("TvRage", strconv.Itoa(result.ExternalIDs.TvrageID))
		}
	}

	rating := float32(result.VoteAverage)
	target.CommunityRating = &rating

	studios := append(namesFrom(result.Networks), namesFrom(result.ProductionCompanies)...)
	if len(studios) > 0 {
		target.Values.Studios = studios
	}
	if genres := trimmedNames(result.Genres); len(genres) > 0 {
		target.Values.Genres = genres
	}
	if result.Keywords != nil && !provider.config.ExcludeTagsSeries {
		if tags := trimmedNames(result.Keywords.Results); len(tags) > 0 {
			target.Values.Tags = tags
		}
	}

	if len(result.EpisodeRunTime) > 0 && result.EpisodeRunTime[0] > 0 {
		ticks := int64(result.EpisodeRunTime[0]) * 60 * 10_000_000
		target.RunTimeTicks = &ticks
	}
	if status, ok := mapSeriesStatus(result.Status); ok {
		target.SeriesStatus = &status
	}
	if end, ok := parseTMDbDate(result.LastAirDate); ok {
		target.EndDate = &end
	}
	if premiere, ok := parseTMDbDate(result.FirstAirDate); ok {
		target.PremiereDate = &premiere
		year := premiere.Year()
		target.ProductionYear = &year
	}

	if official := seriesOfficialRating(result.ContentRatings, country); official != "" {
		setString(&target.OfficialRating, official)
	}

	if trailers := trailers(result.Videos); len(trailers) > 0 {
		target.Extra.RemoteTrailers = trailers
	}

	profile := provider.profileURL(ctx)
	people := mapCastCrew(provider.config, profile, seriesCast(result.Credits), seriesCrew(result.Credits))
	people = append(people, creators(profile, result.CreatedBy)...)
	if len(people) > 0 {
		target.People = people
	}

	provider.applySeriesImages(ctx, target, result, replaceImages)
}

// mapSeriesStatus maps TMDb's status string onto item.SeriesStatus via the
// ported naming helper (TvParserHelpers.TryParseSeriesStatus).
func mapSeriesStatus(status string) (item.SeriesStatus, bool) {
	parsed, ok := naming.TryParseSeriesStatus(status)
	if !ok {
		return 0, false
	}
	switch parsed {
	case naming.SeriesStatusEnded:
		return item.SeriesStatusEnded, true
	case naming.SeriesStatusContinuing:
		return item.SeriesStatusContinuing, true
	case naming.SeriesStatusUnreleased:
		return item.SeriesStatusUnreleased, true
	default:
		return 0, false
	}
}

// seriesOfficialRating selects the content rating for the requested country,
// then US, then the first available (US and first are stored unprefixed).
func seriesOfficialRating(ratings *contentRatings, country string) string {
	if ratings == nil || len(ratings.Results) == 0 {
		return ""
	}
	find := func(iso string) string {
		for _, entry := range ratings.Results {
			if strings.EqualFold(entry.Iso31661, iso) && strings.TrimSpace(entry.Rating) != "" {
				return entry.Rating
			}
		}
		return ""
	}
	if country != "" {
		if rating := find(country); rating != "" {
			return buildParentalRating(country, rating)
		}
	}
	if rating := find("US"); rating != "" {
		return rating
	}
	if rating := strings.TrimSpace(ratings.Results[0].Rating); rating != "" {
		return rating
	}
	return ""
}

// creators maps TMDb created_by entries to Creator credits.
func creators(profile profileURL, entries []createdBy) []item.PersonCredit {
	out := make([]item.PersonCredit, 0, len(entries))
	for _, person := range entries {
		if strings.TrimSpace(person.Name) == "" {
			continue
		}
		name := strings.TrimSpace(person.Name)
		out = append(out, item.PersonCredit{
			PersonID:    library.PersonEntity(name).ID,
			Name:        name,
			Type:        item.PersonKindCreator,
			ImageURL:    profile.url(person.ProfilePath),
			ProviderIDs: personProviderIDs(person.ID),
		})
	}
	return out
}

// personProviderIDs is the PersonInfo.ProviderIds the TMDb providers set on
// a credit: the TMDb person id when it is positive.
func personProviderIDs(tmdbID int) item.ProviderIDs {
	var ids item.ProviderIDs
	if tmdbID > 0 {
		ids.Set("Tmdb", strconv.Itoa(tmdbID))
	}
	return ids
}

func seriesCast(credits *seriesCredits) []castMember {
	if credits == nil {
		return nil
	}
	return credits.Cast
}

func seriesCrew(credits *seriesCredits) []crewMember {
	if credits == nil {
		return nil
	}
	return credits.Crew
}

// applySeriesImages adds remote artwork the same way movies do.
func (provider *Provider) applySeriesImages(ctx context.Context, target *item.Item, result *tvShow, replace bool) {
	base := provider.api.imageBaseURL(ctx)
	size := provider.config.imageSizeOrOriginal()
	add := func(kind item.ImageType, data imageData) {
		if data.FilePath == "" {
			return
		}
		putRemoteImage(target, kind, imageURL(base, size, data.FilePath), data.Width, data.Height, replace)
	}

	// poster_path carries no dimensions; the image collection does.
	primary := imageData{FilePath: result.PosterPath}
	if result.Images != nil && len(result.Images.Posters) > 0 {
		primary = result.Images.Posters[0]
	}
	add(item.ImageTypePrimary, primary)
	if result.Images != nil {
		// Only the first backdrop of each type attaches, so a replacing
		// refresh keeps TMDb's top-ranked one instead of the last.
		added := map[item.ImageType]bool{}
		for _, backdrop := range result.Images.Backdrops {
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
		if len(result.Images.Logos) > 0 {
			add(item.ImageTypeLogo, result.Images.Logos[0])
		}
	}
}

// mapCastCrew is the shared cast/crew selection used by movies and series:
// order cast by billing and bound it, keep only wanted crew kinds bounded, and
// honor the hide-missing-image flags.
func mapCastCrew(config Config, profile profileURL, castMembers []castMember, crewMembers []crewMember) []item.PersonCredit {
	return append(mapCast(config, profile, castMembers, item.PersonKindActor), mapCrew(config, profile, crewMembers)...)
}

// mapCast orders cast by billing, bounds it to MaxCastMembers, and maps it to
// kind. Episodes use it for guest stars as well.
func mapCast(config Config, profile profileURL, castMembers []castMember, kind item.PersonKind) []item.PersonCredit {
	people := make([]item.PersonCredit, 0, len(castMembers))
	cast := append([]castMember(nil), castMembers...)
	if config.HideMissingCastMembers {
		cast = filterCast(cast)
	}
	sort.SliceStable(cast, func(i, j int) bool { return cast[i].Order < cast[j].Order })
	for index, actor := range cast {
		if index >= config.MaxCastMembers {
			break
		}
		if strings.TrimSpace(actor.Name) == "" {
			continue
		}
		name := strings.TrimSpace(actor.Name)
		role := strings.TrimSpace(actor.Character)
		order := actor.Order
		people = append(people, item.PersonCredit{
			PersonID:    library.PersonEntity(name).ID,
			Name:        name,
			Role:        &role,
			Type:        kind,
			SortOrder:   &order,
			ImageURL:    profile.url(actor.ProfilePath),
			ProviderIDs: personProviderIDs(actor.ID),
		})
	}
	return people
}

// mapCrew keeps wanted crew kinds, bounded to MaxCrewMembers.
func mapCrew(config Config, profile profileURL, crewMembers []crewMember) []item.PersonCredit {
	people := make([]item.PersonCredit, 0, len(crewMembers))
	crewCount := 0
	for _, member := range crewMembers {
		kind, wanted := mapCrewToPersonType(member.Department, member.Job)
		if !wanted {
			continue
		}
		if config.HideMissingCrewMembers && strings.TrimSpace(member.ProfilePath) == "" {
			continue
		}
		if crewCount >= config.MaxCrewMembers {
			break
		}
		if strings.TrimSpace(member.Name) == "" {
			continue
		}
		crewCount++
		name := strings.TrimSpace(member.Name)
		role := strings.TrimSpace(member.Job)
		people = append(people, item.PersonCredit{
			PersonID:    library.PersonEntity(name).ID,
			Name:        name,
			Role:        &role,
			Type:        kind,
			ImageURL:    profile.url(member.ProfilePath),
			ProviderIDs: personProviderIDs(member.ID),
		})
	}
	return people
}

// profileURL resolves a TMDb profile_path to an absolute image URL, as
// TmdbClientManager.GetProfileUrl does. A zero value resolves nothing.
type profileURL struct {
	base, size string
}

func (profile profileURL) url(path string) string {
	if profile.base == "" {
		return ""
	}
	return imageURL(profile.base, profile.size, strings.TrimSpace(path))
}

// profileURL returns the resolver for cast and crew profile images. Upstream
// uses the ProfileSize preference, which defaults to "original".
func (provider *Provider) profileURL(ctx context.Context) profileURL {
	return profileURL{base: provider.api.imageBaseURL(ctx), size: provider.config.imageSizeOrOriginal()}
}
