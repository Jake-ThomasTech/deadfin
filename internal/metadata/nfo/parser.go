// SPDX-License-Identifier: GPL-2.0-only

package nfo

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/item"
)

// ParseOptions selects the item family whose NFO-specific fields are accepted.
type ParseOptions struct {
	Kind item.Kind
}

// Person is an NFO person credit. Order is the <order> or <sortorder> value of
// an <actor> element (upstream GetPersonFromXmlNode). It is nil when the NFO
// supplies no order; the credit then keeps its position in People.
type Person struct {
	Name  string
	Role  *string
	Kind  item.PersonKind
	Order *int
}

// Result is the metadata parsed from one NFO document. Patch contains only
// fields established by the document; collection values remain separate so a
// caller can merge them under its own persistence policy.
type Result struct {
	Patch       item.Item
	People      []Person
	ProviderIDs item.ProviderIDs
	Genres      []string
	Studios     []string
	Tags        []string
}

type element struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Text    string     `xml:",chardata"`
	Nodes   []element  `xml:",any"`
}

// Parse reads one Kodi/Jellyfin NFO document. It consumes XML incrementally,
// returns values parsed before malformed trailing data, and only returns an
// error for cancellation or reader failures before XML parsing begins.
func Parse(ctx context.Context, r io.Reader, options ParseOptions) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	decoder := xml.NewDecoder(r)
	var result Result
	result.Patch.Kind = options.Kind
	var root xml.StartElement
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result, nil
			}
			return result, nil
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		root = start
		break
	}

	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return result, nil
			}
			return result, nil
		}
		switch value := token.(type) {
		case xml.StartElement:
			var node element
			if err := decoder.DecodeElement(&node, &value); err != nil {
				return result, nil
			}
			parseElement(&result, options.Kind, node)
		case xml.EndElement:
			if value.Name == root.Name && options.Kind != item.KindEpisode {
				return result, nil
			}
		}
	}
}

func parseElement(result *Result, kind item.Kind, node element) {
	name := strings.ToLower(node.XMLName.Local)
	value := normalized(node.Text)
	switch name {
	case "title", "name", "localtitle":
		setString(&result.Patch.Name, value)
	case "originaltitle":
		setString(&result.Patch.OriginalTitle, value)
	case "sortname":
		setString(&result.Patch.SortName, value)
	case "sorttitle":
		setString(&result.Patch.ForcedSortName, value)
	case "plot", "biography", "review":
		setString(&result.Patch.Overview, value)
	case "tagline":
		setString(&result.Patch.Tagline, value)
	case "language":
		setString(&result.Patch.PreferredMetadataLanguage, value)
	case "countrycode":
		setString(&result.Patch.PreferredMetadataCountryCode, value)
	case "mpaa":
		setString(&result.Patch.OfficialRating, value)
	case "customrating":
		setString(&result.Patch.CustomRating, value)
	case "dateadded":
		setTime(&result.Patch.DateCreated, value)
	case "aired", "formed", "premiered", "releasedate":
		setTime(&result.Patch.PremiereDate, value)
		if result.Patch.ProductionYear == nil && result.Patch.PremiereDate != nil {
			year := result.Patch.PremiereDate.Year()
			result.Patch.ProductionYear = &year
		}
	case "enddate":
		setTime(&result.Patch.EndDate, value)
	case "year":
		if year, ok := integer(value); ok && year > 1850 {
			result.Patch.ProductionYear = &year
		}
	case "runtime":
		parts := strings.Fields(value)
		if len(parts) > 0 {
			if minutes, ok := integer(parts[0]); ok {
				ticks := int64(minutes) * 60 * 10_000_000
				result.Patch.RunTimeTicks = &ticks
			}
		}
	case "rating":
		setRating(&result.Patch.CommunityRating, value, false)
	case "communityrating":
		setRating(&result.Patch.CommunityRating, value, true)
	case "criticrating":
		setRating(&result.Patch.CriticRating, value, false)
	case "ratings":
		for _, child := range node.Nodes {
			if strings.EqualFold(child.XMLName.Local, "rating") {
				parseRating(&result.Patch, child)
			}
		}
	case "lockdata":
		result.Patch.IsLocked = strings.EqualFold(value, "true")
	case "lockedfields":
		result.Patch.LockedFields = parseLockedFields(value)
	case "genre":
		result.Genres = appendSplit(result.Genres, value, "/")
	case "studio":
		result.Studios = appendNonEmpty(result.Studios, value)
	case "tag", "style":
		result.Tags = appendNonEmpty(result.Tags, value)
	case "director":
		result.People = appendPeople(result.People, node, item.PersonKindDirector)
	case "writer":
		result.People = appendPeople(result.People, node, item.PersonKindWriter)
	case "credits":
		for _, person := range appendSplit(nil, value, "/") {
			result.People = append(result.People, Person{Name: person, Kind: item.PersonKindWriter})
		}
	case "actor":
		result.People = appendPeople(result.People, node, item.PersonKindActor)
	case "artist":
		if kind == item.KindMusicVideo || kind == item.KindMusicAlbum || kind == item.KindMusicArtist {
			result.People = appendPeople(result.People, node, item.PersonKindArtist)
		}
	case "albumartist":
		result.People = appendPeople(result.People, node, item.PersonKindAlbumArtist)
	case "album":
		if kind == item.KindMusicVideo || kind == item.KindMusicAlbum {
			setString(&result.Patch.Album, value)
		}
	case "season":
		if kind == item.KindEpisode {
			setInt(&result.Patch.ParentIndexNumber, value)
		}
	case "episode":
		if kind == item.KindEpisode {
			setInt(&result.Patch.IndexNumber, value)
		}
	case "seasonnumber":
		if kind == item.KindSeason {
			setInt(&result.Patch.IndexNumber, value)
		}
	case "seasonname":
		if kind == item.KindSeason {
			setString(&result.Patch.Name, value)
		}
	case "showtitle":
		if kind == item.KindEpisode {
			setString(&result.Patch.SeriesName, value)
		}
	case "airsbefore_episode", "displayepisode":
		if kind == item.KindEpisode {
			setInt(&result.Patch.AirsBeforeEpisodeNumber, value)
		}
	case "airsafter_season", "displayafterseason":
		if kind == item.KindEpisode {
			setInt(&result.Patch.AirsAfterSeasonNumber, value)
		}
	case "airsbefore_season", "displayseason":
		if kind == item.KindEpisode {
			setInt(&result.Patch.AirsBeforeSeasonNumber, value)
		}
	case "status":
		if kind == item.KindSeries {
			if status, ok := seriesStatus(value); ok {
				result.Patch.SeriesStatus = &status
			}
		}
	case "id":
		parseID(result, node, value)
	case "uniqueid":
		if provider := attribute(node, "type"); provider != "" {
			setProvider(result, provider, value)
		}
	case "imdbid", "imdb_id", "tmdbid", "tvdbid", "collectionnumber", "tmdbcolid", "tmdbcol":
		setProvider(result, name, value)
	case "thumb":
		addImage(result, node, value, "poster")
	case "fanart":
		for _, child := range node.Nodes {
			if strings.EqualFold(child.XMLName.Local, "thumb") {
				addImage(result, child, normalized(child.Text), "fanart")
			}
		}
	case "trailer":
		if trailer := youtubeTrailer(value); trailer != "" {
			result.Patch.Extra.RemoteTrailers = append(result.Patch.Extra.RemoteTrailers, item.MediaURL{URL: &trailer})
		}
	case "airs_time":
		if kind == item.KindSeries && value != "" {
			ensureSeries(&result.Patch).AirTime = &value
		}
	case "airs_dayofweek":
		if kind == item.KindSeries {
			ensureSeries(&result.Patch).AirDays = parseAirDays(value)
		}
	case "musicbrainzartistid":
		setProvider(result, "MusicBrainzArtist", value)
	case "musicbrainzalbumid":
		setProvider(result, "MusicBrainzAlbum", value)
	case "musicbrainzreleasegroupid":
		setProvider(result, "MusicBrainzReleaseGroup", value)
	case "albumartistcredits":
		if kind == item.KindMusicAlbum {
			for _, child := range node.Nodes {
				if strings.EqualFold(child.XMLName.Local, "artist") {
					result.People = appendPeople(result.People, child, item.PersonKindAlbumArtist)
				}
			}
		}
	case "episodedetails":
		if kind == item.KindEpisode {
			mergeEpisodeBlock(result, node)
		}
	}
}

func ensureSeries(patch *item.Item) *item.SeriesExtra {
	if patch.Extra.Series == nil {
		patch.Extra.Series = &item.SeriesExtra{}
	}
	return patch.Extra.Series
}

func parseAirDays(value string) []item.DayOfWeek {
	var result []item.DayOfWeek
	for _, value := range strings.FieldsFunc(value, func(character rune) bool { return character == ',' || character == '|' || character == '/' }) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "sunday":
			result = append(result, item.DayOfWeekSunday)
		case "monday":
			result = append(result, item.DayOfWeekMonday)
		case "tuesday":
			result = append(result, item.DayOfWeekTuesday)
		case "wednesday":
			result = append(result, item.DayOfWeekWednesday)
		case "thursday":
			result = append(result, item.DayOfWeekThursday)
		case "friday":
			result = append(result, item.DayOfWeekFriday)
		case "saturday":
			result = append(result, item.DayOfWeekSaturday)
		}
	}
	return result
}

func mergeEpisodeBlock(result *Result, node element) {
	var next Result
	next.Patch.Kind = item.KindEpisode
	for _, child := range node.Nodes {
		parseElement(&next, item.KindEpisode, child)
	}
	joinEpisodeText(&result.Patch.Name, next.Patch.Name)
	joinEpisodeText(&result.Patch.OriginalTitle, next.Patch.OriginalTitle)
	joinEpisodeText(&result.Patch.Overview, next.Patch.Overview)
	if next.Patch.IndexNumber != nil {
		if result.Patch.Extra.Episode == nil {
			result.Patch.Extra.Episode = &item.EpisodeExtra{}
		}
		if result.Patch.Extra.Episode.IndexNumberEnd == nil || *next.Patch.IndexNumber > *result.Patch.Extra.Episode.IndexNumberEnd {
			index := *next.Patch.IndexNumber
			result.Patch.Extra.Episode.IndexNumberEnd = &index
		}
	}
}

func joinEpisodeText(destination **string, source *string) {
	if source == nil || *source == "" {
		return
	}
	if *destination == nil || **destination == "" {
		value := *source
		*destination = &value
		return
	}
	value := **destination + " / " + *source
	*destination = &value
}

func youtubeTrailer(value string) string {
	const legacy = "plugin://plugin.video.youtube/?action=play_video&videoid="
	const current = "plugin://plugin.video.youtube/play/?video_id="
	switch {
	case strings.HasPrefix(strings.ToLower(value), legacy):
		return "https://www.youtube.com/watch?v=" + value[len(legacy):]
	case strings.HasPrefix(strings.ToLower(value), current):
		return "https://www.youtube.com/watch?v=" + value[len(current):]
	default:
		return ""
	}
}

func parseID(result *Result, node element, value string) {
	setProvider(result, "tmdb", attribute(node, "tmdb"))
	setProvider(result, "tvdb", attribute(node, "tvdb"))
	setProvider(result, "imdb", attribute(node, "imdb"))
	if attribute(node, "imdb") == "" && strings.HasPrefix(value, "tt") {
		setProvider(result, "imdb", value)
	}
}

func parseRating(patch *item.Item, node element) {
	name := attribute(node, "name")
	for _, child := range node.Nodes {
		if strings.EqualFold(child.XMLName.Local, "value") {
			critic := strings.Contains(strings.ToLower(name), "tomato") && !strings.Contains(strings.ToLower(name), "audience") && !strings.Contains(strings.ToLower(name), "avg")
			if critic {
				setRating(&patch.CriticRating, normalized(child.Text), false)
			} else {
				setRating(&patch.CommunityRating, normalized(child.Text), false)
			}
		}
	}
}

func appendPeople(people []Person, node element, kind item.PersonKind) []Person {
	name, role := normalized(node.Text), ""
	var order *int
	for _, child := range node.Nodes {
		switch strings.ToLower(child.XMLName.Local) {
		case "name":
			name = normalized(child.Text)
		case "role":
			role = normalized(child.Text)
		case "order", "sortorder":
			if parsed, ok := integer(normalized(child.Text)); ok {
				order = &parsed
			}
		}
	}
	if name == "" {
		return people
	}
	person := Person{Name: name, Kind: kind, Order: order}
	if role != "" {
		person.Role = &role
	}
	return append(people, person)
}

func addImage(result *Result, node element, value, fallback string) {
	if value == "" || strings.Contains(attribute(node, "aspect"), ".") {
		return
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Scheme == "file" {
		return
	}
	kind := imageType(attribute(node, "aspect"), fallback)
	for _, image := range result.Patch.ImageInfos {
		if image.Type == kind {
			return
		}
	}
	result.Patch.ImageInfos = append(result.Patch.ImageInfos, item.ImageInfo{Type: kind, Index: 0, Path: parsed.String()})
}

func imageType(value, fallback string) item.ImageType {
	if value == "" {
		value = fallback
	}
	switch strings.ToLower(value) {
	case "banner":
		return item.ImageTypeBanner
	case "clearlogo":
		return item.ImageTypeLogo
	case "discart":
		return item.ImageTypeDisc
	case "landscape":
		return item.ImageTypeThumb
	case "clearart":
		return item.ImageTypeArt
	case "fanart":
		return item.ImageTypeBackdrop
	default:
		return item.ImageTypePrimary
	}
}

func setProvider(result *Result, name, value string) {
	if value == "" {
		return
	}
	switch strings.ToLower(name) {
	case "imdb_id", "imdbid", "imdb":
		name = "Imdb"
	case "tmdbid", "tmdb":
		name = "Tmdb"
	case "tvdbid", "tvdb":
		name = "Tvdb"
	case "collectionnumber", "tmdbcolid", "tmdbcol":
		name = "TmdbCollection"
	}
	result.ProviderIDs.Set(name, value)
	result.Patch.ProviderIDs.Set(name, value)
}

func parseLockedFields(value string) []item.MetadataField {
	var fields []item.MetadataField
	for _, name := range strings.Split(value, "|") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "cast":
			fields = append(fields, item.MetadataFieldCast)
		case "genres":
			fields = append(fields, item.MetadataFieldGenres)
		case "productionlocations":
			fields = append(fields, item.MetadataFieldProductionLocations)
		case "studios":
			fields = append(fields, item.MetadataFieldStudios)
		case "tags":
			fields = append(fields, item.MetadataFieldTags)
		case "name":
			fields = append(fields, item.MetadataFieldName)
		case "overview":
			fields = append(fields, item.MetadataFieldOverview)
		case "runtime":
			fields = append(fields, item.MetadataFieldRuntime)
		case "officialrating":
			fields = append(fields, item.MetadataFieldOfficialRating)
		}
	}
	return fields
}

func seriesStatus(value string) (item.SeriesStatus, bool) {
	switch strings.ToLower(value) {
	case "continuing":
		return item.SeriesStatusContinuing, true
	case "ended":
		return item.SeriesStatusEnded, true
	case "unreleased":
		return item.SeriesStatusUnreleased, true
	}
	return 0, false
}

func setString(destination **string, value string) {
	if value != "" {
		*destination = &value
	}
}

func setInt(destination **int, value string) {
	if number, ok := integer(value); ok {
		*destination = &number
	}
}

func setTime(destination **time.Time, value string) {
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			*destination = &parsed
			return
		}
	}
}

func setRating(destination **float32, value string, bounded bool) {
	parsed, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 32)
	if err != nil || bounded && (parsed < 0 || parsed > 10) {
		return
	}
	rating := float32(parsed)
	*destination = &rating
}

func integer(value string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	return parsed, err == nil
}

func attribute(node element, name string) string {
	for _, attribute := range node.Attrs {
		if strings.EqualFold(attribute.Name.Local, name) {
			return normalized(attribute.Value)
		}
	}
	return ""
}

func normalized(value string) string { return strings.TrimSpace(value) }

func appendNonEmpty(values []string, value string) []string {
	if value != "" {
		return append(values, value)
	}
	return values
}

func appendSplit(values []string, value, separator string) []string {
	for _, part := range strings.Split(value, separator) {
		values = appendNonEmpty(values, normalized(part))
	}
	return values
}
