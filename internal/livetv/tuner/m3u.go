// SPDX-License-Identifier: GPL-2.0-only

package tuner

import (
	"bufio"
	"crypto/md5" // #nosec G501 -- upstream channel ids are MD5 digests, not a security control.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/deadfin-org/deadfin/internal/item"
)

// M3UChannelIDPrefix starts every M3U channel id (M3UTunerHost.cs:64-67,
// BaseTunerHost.ChannelIdPrefix = Type + "_").
const M3UChannelIDPrefix = "m3u_"

// maxM3ULine bounds one playlist line. Real m3u_plus lines are a few hundred
// bytes; a longer line is a broken source, not a channel.
const maxM3ULine = 1 << 20

// m3uAttribute is KeyValueRegex in M3uParser.cs:36-37.
var m3uAttribute = regexp.MustCompile(`(?i)([a-z0-9\-_]+)="([^"]+)"`)

// ParseOptions changes how ParseM3U reads a playlist.
type ParseOptions struct {
	// SkipVOD drops movie and series entries of Xtream-style playlists
	// (docs/spikes/livetv.md §4.3). Upstream imports every entry; false is
	// parity.
	SkipVOD bool
}

// M3UChannelIDPrefixFor returns the channel id prefix of an M3U tuner:
// "m3u_" + md5(tuner URL) as a .NET Guid "N" string (M3UTunerHost.cs:64-67).
func M3UChannelIDPrefixFor(tunerURL string) string {
	return M3UChannelIDPrefix + DotNetMD5(tunerURL)
}

// ParseM3U reads an M3U playlist line by line and calls yield for each
// channel, in playlist order. It holds only the current entry, so memory does
// not grow with the playlist. It ports M3uParser.GetChannelsAsync
// (M3uParser.cs:72-114).
func ParseM3U(r io.Reader, channelIDPrefix, tunerHostID string, options ParseOptions, yield func(Channel) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxM3ULine)
	extInf := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case hasPrefixFold(line, "#EXTM3U"):
			continue
		case hasPrefixFold(line, "#EXTINF:"):
			extInf = strings.TrimSpace(line[len("#EXTINF:"):])
			continue
		case strings.TrimSpace(extInf) == "" || strings.HasPrefix(line, "#"):
			continue
		}
		path := line
		current := extInf
		extInf = ""
		if !isValidChannelURL(path) {
			continue
		}
		if options.SkipVOD && isVODPath(path) {
			continue
		}
		channel := channelInfo(current, tunerHostID, path)
		channel.ID = channelIDPrefix + DotNetMD5(path)
		channel.Path = path
		if err := yield(channel); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("m3u: line longer than %d bytes", maxM3ULine)
		}
		return fmt.Errorf("m3u: read: %w", err)
	}
	return nil
}

// channelInfo ports M3uParser.GetChannelInfo (M3uParser.cs:116-168). The id
// it builds from channel-id and tvg-id there is overwritten by the caller
// (M3uParser.cs:104), so it is not computed here.
func channelInfo(extInf, tunerHostID, mediaURL string) Channel {
	channel := Channel{TunerHostID: tunerHostID, Type: ChannelTypeTV}
	attributes, remaining := parseExtInf(strings.TrimSpace(extInf))
	if logo, ok := attributes.get("tvg-logo"); ok {
		channel.ImageURL = logo
	} else if logo, ok := attributes.get("logo"); ok {
		channel.ImageURL = logo
	}
	if group, ok := attributes.get("group-title"); ok {
		channel.Group = group
	}
	channel.Name = channelName(remaining, attributes)
	channel.Number = channelNumber(remaining, attributes, mediaURL)
	tvgID, _ := attributes.get("tvg-id")
	channelID, _ := attributes.get("channel-id")
	if strings.TrimSpace(tvgID) == "" {
		channel.TunerChannelID = channelID
	} else {
		channel.TunerChannelID = tvgID
	}
	return channel
}

// lastNamePart returns the text after the last comma, trimmed, when the
// EXTINF holds at least two non-empty comma-separated parts
// (Split(',', RemoveEmptyEntries), M3uParser.cs:172-173, 281-282).
func lastNamePart(extInf string) (string, bool) {
	parts := 0
	last := ""
	for part := range strings.SplitSeq(extInf, ",") {
		if part == "" {
			continue
		}
		parts++
		last = part
	}
	if parts < 2 {
		return "", false
	}
	return strings.TrimSpace(last), true
}

// satIPNumber splits a SatIP-style "84. VOX" or "84.0 - VOX" name into its
// number and the rest (M3uParser.cs:209-224, 284-298).
func satIPNumber(name string) (number, rest string, ok bool) {
	index := strings.IndexByte(name, ' ')
	if index <= 0 {
		return "", "", false
	}
	candidate := strings.Trim(name[:index], " .")
	if _, parsed := item.ParseInvariantDouble(candidate); !parsed {
		return "", "", false
	}
	return candidate, strings.Trim(name[index+1:], " -"), true
}

// channelName ports M3uParser.GetChannelName (M3uParser.cs:279-320).
func channelName(extInf string, attributes extInfAttributes) string {
	name, _ := lastNamePart(extInf)
	if strings.TrimSpace(name) != "" {
		if _, rest, ok := satIPNumber(name); ok {
			name = rest
		}
	}
	if strings.TrimSpace(name) == "" {
		name, _ = attributes.get("tvg-name")
	}
	if strings.TrimSpace(name) == "" {
		name, _ = attributes.get("tvg-id")
	}
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return name
}

// channelNumber ports M3uParser.GetChannelNumber (M3uParser.cs:170-255),
// including its order: tvg-chno, then tvg-id or (only when tvg-id is
// present) channel-id, then a SatIP number in the name, then the media URL
// file name.
func channelNumber(extInf string, attributes extInfAttributes, mediaURL string) string {
	nameInExtInf, _ := lastNamePart(extInf)
	number := ""
	hasNumber := false
	if value, ok := attributes.get("tvg-chno"); ok {
		if _, parsed := item.ParseInvariantDouble(value); parsed {
			number, hasNumber = value, true
		}
	}
	if !isValidChannelNumber(number, hasNumber) {
		if tvgID, ok := attributes.get("tvg-id"); ok {
			if _, parsed := item.ParseInvariantDouble(tvgID); parsed {
				number, hasNumber = tvgID, true
			} else if channelID, ok := attributes.get("channel-id"); ok {
				if _, parsed := item.ParseInvariantDouble(channelID); parsed {
					number, hasNumber = channelID, true
				}
			}
		}
		if strings.TrimSpace(number) == "" && strings.TrimSpace(nameInExtInf) != "" {
			if satIP, _, ok := satIPNumber(nameInExtInf); ok {
				number, hasNumber = satIP, true
			}
		}
	}
	if !isValidChannelNumber(number, hasNumber) {
		number, hasNumber = "", false
	}
	if hasNumber && strings.TrimSpace(number) != "" {
		return strings.TrimSpace(number)
	}
	if strings.TrimSpace(mediaURL) == "" {
		return ""
	}
	fileName := fileNameWithoutExtension(rightPart(mediaURL, '/'))
	if !isValidChannelNumber(fileName, true) {
		return ""
	}
	return fileName
}

// isValidChannelNumber ports M3uParser.IsValidChannelNumber
// (M3uParser.cs:267-277). present distinguishes a null C# string.
func isValidChannelNumber(number string, present bool) bool {
	if !present || strings.TrimSpace(number) == "" || number == "-1" || number == "0" {
		return false
	}
	_, ok := item.ParseInvariantDouble(number)
	return ok
}

// isValidChannelURL ports M3uParser.IsValidChannelUrl (M3uParser.cs:257-265):
// an absolute URI with scheme http, https, rtsp, rtp or udp.
func isValidChannelURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed.Host != ""
	case "rtsp", "rtp", "udp":
		return true
	}
	return false
}

// isVODPath reports an Xtream movie or series entry: a /movie/ or /series/
// path segment, or a file extension of a finite media file.
func isVODPath(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.ToLower(parsed.Path)
	if strings.Contains(path, "/movie/") || strings.Contains(path, "/series/") {
		return true
	}
	if dot := strings.LastIndexByte(path, '.'); dot >= 0 && !strings.Contains(path[dot:], "/") {
		switch path[dot+1:] {
		case "mkv", "mp4", "m4v", "avi", "mov", "wmv", "flv", "webm", "mpg", "mpeg":
			return true
		}
	}
	return false
}

// rightPart ports Jellyfin.Extensions StringExtensions.RightPart: the text
// after the last needle, the whole text without one, empty when it ends
// with one.
func rightPart(value string, needle byte) string {
	index := strings.LastIndexByte(value, needle)
	if index < 0 {
		return value
	}
	return value[index+1:]
}

// fileNameWithoutExtension ports Path.GetFileNameWithoutExtension on Unix
// for a value that holds no '/'.
func fileNameWithoutExtension(name string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[:dot]
	}
	return name
}

// extInfAttributes keeps the attribute order-insensitive, case-insensitive
// lookup of M3uParser.ParseExtInf; a later duplicate wins.
type extInfAttributes map[string]string

func (attributes extInfAttributes) get(key string) (string, bool) {
	value, ok := attributes[strings.ToLower(key)]
	return value, ok
}

// parseExtInf ports M3uParser.ParseExtInf (M3uParser.cs:322-339): it returns
// the attributes and the line with every key="value" occurrence removed.
func parseExtInf(line string) (extInfAttributes, string) {
	attributes := make(extInfAttributes)
	remaining := line
	for _, match := range m3uAttribute.FindAllStringSubmatch(line, -1) {
		key, value := match[1], match[2]
		attributes[strings.ToLower(key)] = value
		remaining = replaceAllFold(remaining, key+`="`+value+`"`)
	}
	return attributes, remaining
}

// replaceAllFold removes every case-insensitive occurrence of pattern, as
// string.Replace(pattern, "", OrdinalIgnoreCase) does.
func replaceAllFold(value, pattern string) string {
	if pattern == "" {
		return value
	}
	var out strings.Builder
	for {
		index := indexFold(value, pattern)
		if index < 0 {
			out.WriteString(value)
			return out.String()
		}
		out.WriteString(value[:index])
		value = value[index+len(pattern):]
	}
}

func indexFold(value, pattern string) int {
	for index := 0; index+len(pattern) <= len(value); index++ {
		if strings.EqualFold(value[index:index+len(pattern)], pattern) {
			return index
		}
	}
	return -1
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

// DotNetMD5 ports string.GetMD5().ToString("N"): MD5 of the UTF-16LE text,
// formatted with .NET Guid's mixed-endian byte order.
func DotNetMD5(value string) string {
	units := utf16.Encode([]rune(value))
	encoded := make([]byte, len(units)*2)
	for index, unit := range units {
		encoded[index*2] = byte(unit)
		encoded[index*2+1] = byte(unit >> 8)
	}
	d := md5.Sum(encoded) // #nosec G401 -- upstream id derivation.
	return hex.EncodeToString([]byte{d[3], d[2], d[1], d[0], d[5], d[4], d[7], d[6], d[8], d[9], d[10], d[11], d[12], d[13], d[14], d[15]})
}
