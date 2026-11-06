// SPDX-License-Identifier: GPL-2.0-only

package guide

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// EtagPrefix starts every XMLTV program etag (XmlTvProgramEtag.cs).
const EtagPrefix = "xmltv-sha256-v1:"

// IsEtag reports an XMLTV-style etag.
func IsEtag(etag string) bool {
	return strings.TrimSpace(etag) != "" && strings.HasPrefix(etag, EtagPrefix)
}

// EtagMatchesStored reports that an incoming XMLTV etag equals the stored
// one, so the stored program needs no update. Other etag schemes never
// match (XmlTvProgramEtag.MatchesStored).
func EtagMatchesStored(incoming, stored string) bool {
	return IsEtag(incoming) && strings.EqualFold(incoming, stored)
}

// Etag ports XmlTvProgramEtag.TryCreate: a SHA-256 over every Program field
// the guide refresh stores, in a fixed order. The result is byte-equal to
// upstream's, so an imported database keeps its program etags.
func Etag(program Program) (string, error) {
	switch {
	case strings.TrimSpace(program.ID) == "":
		return "", errors.New("program id is empty")
	case strings.TrimSpace(program.ChannelID) == "":
		return "", errors.New("channel id is empty")
	case program.StartDate.IsZero():
		return "", errors.New("start date is empty")
	case program.EndDate.IsZero():
		return "", errors.New("end date is empty")
	case !program.EndDate.After(program.StartDate):
		return "", errors.New("end date is not after start date")
	}

	var b strings.Builder
	b.Grow(1024)
	appendString(&b, "schema", ptr("xmltv-programinfo-v1"))
	appendString(&b, "Id", ptr(program.ID))
	appendString(&b, "ChannelId", ptr(program.ChannelID))
	appendString(&b, "Name", program.Name)
	appendString(&b, "OfficialRating", program.OfficialRating)
	appendString(&b, "Overview", program.Overview)
	appendString(&b, "StartDate", ptr(formatDotNetO(program.StartDate)))
	appendString(&b, "EndDate", ptr(formatDotNetO(program.EndDate)))
	appendString(&b, "Genres.Count", ptr(strconv.Itoa(len(program.Genres))))
	for index, genre := range program.Genres {
		appendString(&b, "Genres["+strconv.Itoa(index)+"]", ptr(genre))
	}
	var originalAirDate *string
	if program.OriginalAirDate != nil {
		originalAirDate = ptr(formatDotNetO(*program.OriginalAirDate))
	}
	appendString(&b, "OriginalAirDate", originalAirDate)
	appendString(&b, "IsHD", nil)
	appendString(&b, "Audio", nil)
	var communityRating *string
	if program.CommunityRating != nil {
		communityRating = ptr(formatFloatR(*program.CommunityRating))
	}
	appendString(&b, "CommunityRating", communityRating)
	appendBool(&b, "IsRepeat", program.IsRepeat)
	appendString(&b, "EpisodeTitle", program.EpisodeTitle)
	appendString(&b, "ImagePath", nil)
	appendString(&b, "ImageUrl", program.ImageURL)
	appendString(&b, "ThumbImageUrl", program.ThumbImageURL)
	appendString(&b, "LogoImageUrl", nil)
	appendString(&b, "BackdropImageUrl", program.BackdropImageURL)
	appendBool(&b, "IsMovie", program.IsMovie)
	appendBool(&b, "IsSports", program.IsSports)
	appendBool(&b, "IsSeries", program.IsSeries)
	appendBool(&b, "IsLive", program.IsLive)
	appendBool(&b, "IsNews", program.IsNews)
	appendBool(&b, "IsKids", program.IsKids)
	appendBool(&b, "IsPremiere", program.IsPremiere)
	appendInt(&b, "ProductionYear", program.ProductionYear)
	appendString(&b, "SeriesId", program.SeriesID)
	appendString(&b, "ShowId", program.ShowID)
	appendInt(&b, "SeasonNumber", program.SeasonNumber)
	appendInt(&b, "EpisodeNumber", program.EpisodeNumber)
	appendDictionary(&b, "ProviderIds", nil)
	appendDictionary(&b, "SeriesProviderIds", nil)

	sum := sha256.Sum256([]byte(b.String()))
	return EtagPrefix + strings.ToUpper(hex.EncodeToString(sum[:])), nil
}

func ptr(value string) *string { return &value }

// appendString writes "name|S|<UTF-16 length>|value\n" or "name|N|0|\n"
// (XmlTvProgramEtag.AppendValue). The length is .NET string.Length.
func appendString(b *strings.Builder, name string, value *string) {
	b.WriteString(name)
	b.WriteByte('|')
	if value == nil {
		b.WriteString("N|0|")
	} else {
		b.WriteString("S|")
		b.WriteString(strconv.Itoa(len(utf16.Encode([]rune(*value)))))
		b.WriteByte('|')
		b.WriteString(*value)
	}
	b.WriteByte('\n')
}

func appendBool(b *strings.Builder, name string, value bool) {
	if value {
		appendString(b, name, ptr("true"))
	} else {
		appendString(b, name, ptr("false"))
	}
}

func appendInt(b *strings.Builder, name string, value *int) {
	if value == nil {
		appendString(b, name, nil)
		return
	}
	appendString(b, name, ptr(strconv.Itoa(*value)))
}

func appendDictionary(b *strings.Builder, name string, values map[string]string) {
	appendString(b, name+".Count", ptr(strconv.Itoa(len(values))))
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if a, c := strings.ToUpper(keys[i]), strings.ToUpper(keys[j]); a != c {
			return a < c
		}
		return keys[i] < keys[j]
	})
	for index, key := range keys {
		value := values[key]
		appendString(b, name+"["+strconv.Itoa(index)+"].Key", ptr(key))
		appendString(b, name+"["+strconv.Itoa(index)+"].Value", ptr(value))
	}
}

// formatDotNetO formats a UTC DateTime as .NET "O":
// yyyy-MM-ddTHH:mm:ss.fffffffZ.
func formatDotNetO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.0000000Z")
}

// formatFloatR formats a float32 as .NET float.ToString("R"): the shortest
// round-trip text. .NET writes exponents as E+XX; guide ratings never need
// one.
func formatFloatR(value float32) string {
	return strconv.FormatFloat(float64(value), 'G', -1, 32)
}
