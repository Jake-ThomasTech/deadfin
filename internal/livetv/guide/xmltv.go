// SPDX-License-Identifier: GPL-2.0-only

package guide

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html/charset"

	"github.com/deadfin-org/deadfin/internal/item"
)

// The reader ports XmlTvReader of the Jellyfin.XmlTv package that upstream
// pins (10.12.0-pre1; github.com/jellyfin/jellyfin.xmltv at 6c8c302,
// src/Jellyfin.XmlTv/XmlTvReader.cs). Line references below are to that
// file. It reads only the elements that XmlTvListingsProvider maps
// (XmlTvListingsProvider.cs:201-280).

// xmltvDate is DateWithOffsetRegex (XmlTvReader.cs:33).
var xmltvDate = regexp.MustCompile(`^([0-9]{4,14})(\s([+-]*[0-9]{1,4}))?$`)

// sxxexx is the SxxExx episode-num pattern (XmlTvReader.cs:637-658).
var sxxexx = regexp.MustCompile(`(?i)s([0-9]+)e([0-9]+)`)

// XMLTVChannel is one <channel> (XmlTvChannel).
type XMLTVChannel struct {
	ID          string
	DisplayName string
	Number      string
	IconURL     string
}

// XMLTVProgramme is one <programme> with the members the provider maps
// (XmlTvProgram).
type XMLTVProgramme struct {
	ChannelID         string
	StartDate         time.Time // keeps the file's UTC offset
	EndDate           time.Time
	Title             *string
	Description       *string
	EpisodeTitle      *string
	HasEpisode        bool
	Series            *int
	Episode           *int
	ProgramID         string
	Categories        []string
	IsPreviouslyShown bool
	IsNew             bool
	IsLive            bool
	CopyrightDate     *time.Time
	Premiere          bool
	FirstIcon         *string // Source of the first icon; nil when none
	StillPath         *string
	BackdropPath      *string
	FirstRating       *string // Value of the first rating, which may be nil
	HasRating         bool
}

// Listings receives what one pass over an XMLTV file finds.
type Listings struct {
	// Channel is called for each channel in file order.
	Channel func(XMLTVChannel) error
	// WantProgrammes reports whether programmes of a channel id are needed.
	// The match is the caller's; XmlTvReader compares ids ignoring case.
	WantProgrammes func(channelID string) bool
	// Programme is called for each wanted programme that overlaps
	// [Start, End) in file order (XmlTvReader.cs:204).
	Programme func(XMLTVProgramme) error
	Start     time.Time
	End       time.Time
}

// ReadXMLTV reads an XMLTV document once. language selects among
// lang-tagged alternatives (XmlTvReader.ProcessNode). Memory holds one
// <channel> or <programme> element at a time.
func ReadXMLTV(ctx context.Context, r io.Reader, language *string, listings Listings) error {
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = charset.NewReaderLabel
	inTV := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("xmltv: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case !inTV:
			if start.Name.Local == "tv" {
				inTV = true
			}
		case start.Name.Local == "channel":
			element, err := readNode(decoder, start)
			if err != nil {
				return fmt.Errorf("xmltv: %w", err)
			}
			if listings.Channel == nil {
				continue
			}
			if channel, ok := parseChannel(element, language); ok {
				if err := listings.Channel(channel); err != nil {
					return err
				}
			}
		case start.Name.Local == "programme":
			if listings.Programme == nil || !wantedProgramme(start, listings) {
				if err := decoder.Skip(); err != nil {
					return fmt.Errorf("xmltv: %w", err)
				}
				continue
			}
			element, err := readNode(decoder, start)
			if err != nil {
				return fmt.Errorf("xmltv: %w", err)
			}
			if err := listings.Programme(parseProgramme(element, language)); err != nil {
				return err
			}
		}
	}
}

// wantedProgramme ports the header checks of GetProgramme
// (XmlTvReader.cs:191-207): a channel attribute that the caller wants, and a
// programme that overlaps the window. A missing or bad stop is MinValue, so
// the programme is dropped.
func wantedProgramme(start xml.StartElement, listings Listings) bool {
	channel, ok := attribute(start.Attr, "channel")
	if !ok || (listings.WantProgrammes != nil && !listings.WantProgrammes(channel)) {
		return false
	}
	begin, end := headerDate(start.Attr, "start"), headerDate(start.Attr, "stop")
	return !end.Before(listings.Start) && begin.Before(listings.End)
}

// headerDate ports PopulateHeader (XmlTvReader.cs:1223-1244).
func headerDate(attrs []xml.Attr, name string) time.Time {
	value, _ := attribute(attrs, name)
	if parsed, ok := ParseXMLTVDate(value); ok {
		return parsed
	}
	return dotNetMinValue
}

// dotNetMinValue is DateTimeOffset.MinValue.
var dotNetMinValue = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// ParseXMLTVDate ports XmlTvReader.ParseDate (XmlTvReader.cs:1251-1315):
// 4 to 14 digits, padded from 20000101000000, with an optional offset that
// counts only when it is exactly five characters (+HHMM); otherwise UTC.
func ParseXMLTVDate(value string) (time.Time, bool) {
	match := xmltvDate.FindStringSubmatch(value)
	if match == nil {
		return time.Time{}, false
	}
	digits := match[1]
	const complete = "20000101000000"
	if len(digits) < len(complete) {
		digits += complete[len(digits):]
	}
	location := time.UTC
	if offset := match[3]; len(offset) == 5 {
		sign := offset[0]
		hours, errH := strconv.Atoi(offset[1:3])
		minutes, errM := strconv.Atoi(offset[3:5])
		if (sign != '+' && sign != '-') || errH != nil || errM != nil || minutes > 59 || hours*60+minutes > 14*60 {
			return time.Time{}, false
		}
		seconds := hours*3600 + minutes*60
		if sign == '-' {
			seconds = -seconds
		}
		location = time.FixedZone("", seconds)
	}
	parsed, err := time.ParseInLocation("20060102150405", digits, location)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

// node is one element with its text and child elements.
type node struct {
	name     string
	attrs    []xml.Attr
	text     strings.Builder
	children []*node
}

func readNode(decoder *xml.Decoder, start xml.StartElement) (*node, error) {
	element := &node{name: start.Name.Local, attrs: start.Attr}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch t := token.(type) {
		case xml.StartElement:
			child, err := readNode(decoder, t)
			if err != nil {
				return nil, err
			}
			element.children = append(element.children, child)
		case xml.CharData:
			element.text.Write(t)
		case xml.EndElement:
			return element, nil
		}
	}
}

func attribute(attrs []xml.Attr, name string) (string, bool) {
	for _, attr := range attrs {
		if attr.Name.Local == name && attr.Name.Space == "" {
			return attr.Value, true
		}
	}
	return "", false
}

func optionalAttribute(attrs []xml.Attr, name string) *string {
	if value, ok := attribute(attrs, name); ok {
		return &value
	}
	return nil
}

// run returns the elements from index i that share its exact name; non-element
// nodes between them do not end the run (ProcessNode, XmlTvReader.cs:1121-1141).
func run(children []*node, i int) []*node {
	j := i + 1
	for j < len(children) && children[j].name == children[i].name {
		j++
	}
	return children[i:j]
}

// selectLanguage ports the choice of ProcessNode (XmlTvReader.cs:1143-1167):
// the first value whose lang equals language ignoring case (a nil language
// matches only a missing lang), else the first with a blank or missing lang,
// else the first.
func selectLanguage(nodes []*node, language *string) string {
	for _, n := range nodes {
		lang := optionalAttribute(n.attrs, "lang")
		if (language == nil && lang == nil) || (language != nil && lang != nil && strings.EqualFold(*language, *lang)) {
			return n.text.String()
		}
	}
	for _, n := range nodes {
		if lang := optionalAttribute(n.attrs, "lang"); lang == nil || strings.TrimSpace(*lang) == "" {
			return n.text.String()
		}
	}
	return nodes[0].text.String()
}

// selectMultiple ports ProcessMultipleNodes (XmlTvReader.cs:1170-1221): when
// any value has lang equal to language (exact case; nil matches a missing
// lang), only those values; otherwise all values.
func selectMultiple(nodes []*node, language *string) []string {
	matches := func(n *node) bool {
		lang := optionalAttribute(n.attrs, "lang")
		if language == nil || lang == nil {
			return language == nil && lang == nil
		}
		return *language == *lang
	}
	filter := false
	for _, n := range nodes {
		if matches(n) {
			filter = true
			break
		}
	}
	values := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if !filter || matches(n) {
			values = append(values, n.text.String())
		}
	}
	return values
}

// parseChannel ports GetChannel (XmlTvReader.cs:77-131). The child switch
// matches names with exact case.
func parseChannel(element *node, language *string) (XMLTVChannel, bool) {
	id, _ := attribute(element.attrs, "id")
	if id == "" {
		return XMLTVChannel{}, false
	}
	channel := XMLTVChannel{ID: id}
	firstIcon := true
	for i := 0; i < len(element.children); {
		child := element.children[i]
		switch child.name {
		case "display-name":
			names := run(element.children, i)
			channel.DisplayName = selectLanguage(names, language)
			// SetChannelNumber sees every display-name; the last numeric
			// one wins (XmlTvReader.cs:133-140).
			for _, name := range names {
				candidate := strings.NewReplacer("-", ".", "_", ".").Replace(name.text.String())
				if _, ok := item.ParseInvariantDouble(candidate); ok {
					channel.Number = candidate
				}
			}
			i += len(names)
			continue
		case "icon":
			if source, ok := iconSource(child); ok && firstIcon {
				firstIcon = false
				if source != nil {
					channel.IconURL = *source
				}
			}
		}
		i++
	}
	if channel.DisplayName == "" {
		return XMLTVChannel{}, false
	}
	return channel, true
}

// iconSource ports ProcessIconNode (XmlTvReader.cs:940-979). An icon counts
// when src is set or width or height is an integer; ok reports that. The
// source is nil when src is empty.
func iconSource(element *node) (source *string, ok bool) {
	if src, _ := attribute(element.attrs, "src"); src != "" {
		source = &src
		ok = true
	}
	for _, name := range []string{"width", "height"} {
		if value, _ := attribute(element.attrs, name); value != "" {
			if _, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
				ok = true
			}
		}
	}
	return source, ok
}

// parseProgramme ports the child switch of GetProgramme
// (XmlTvReader.cs:219-305). Element names are matched lower-cased, as the
// switch does; runs of the same element compare the exact name.
func parseProgramme(element *node, language *string) XMLTVProgramme {
	channel, _ := attribute(element.attrs, "channel")
	programme := XMLTVProgramme{
		ChannelID:  channel,
		StartDate:  headerDate(element.attrs, "start"),
		EndDate:    headerDate(element.attrs, "stop"),
		Categories: []string{},
	}
	firstIcon := true
	for i := 0; i < len(element.children); {
		child := element.children[i]
		switch strings.ToLower(child.name) {
		case "title", "desc", "sub-title", "premiere":
			nodes := run(element.children, i)
			value := selectLanguage(nodes, language)
			switch strings.ToLower(child.name) {
			case "title":
				programme.Title = &value
			case "desc":
				programme.Description = &value
			case "sub-title":
				programme.HasEpisode = true
				programme.EpisodeTitle = &value
			case "premiere":
				programme.Premiere = true
			}
			i += len(nodes)
			continue
		case "category":
			nodes := run(element.children, i)
			programme.Categories = append(programme.Categories, selectMultiple(nodes, language)...)
			i += len(nodes)
			continue
		case "new":
			programme.IsNew = true
		case "live":
			programme.IsLive = true
		case "previously-shown":
			// XmlTvReader.cs:833-852.
			if value, _ := attribute(child.attrs, "start"); value != "" {
				shown, ok := ParseXMLTVDate(value)
				if !ok || !shown.Equal(programme.StartDate) {
					programme.IsPreviouslyShown = true
				}
			} else {
				programme.IsPreviouslyShown = true
			}
		case "episode-num":
			programme.HasEpisode = true
			parseEpisodeNum(child, &programme)
		case "date":
			// XmlTvReader.cs:351-366.
			text := child.text.String()
			if text == "" {
				programme.CopyrightDate = nil
			} else if parsed, ok := ParseXMLTVDate(text); ok {
				programme.CopyrightDate = &parsed
			}
		case "rating":
			value := ratingValue(child)
			if value.discard {
				break
			}
			if !programme.HasRating {
				programme.HasRating = true
				programme.FirstRating = value.value
			}
		case "icon":
			if source, ok := iconSource(child); ok && firstIcon {
				firstIcon = false
				programme.FirstIcon = source
			}
		case "image":
			kind, _ := attribute(child.attrs, "type")
			path := child.text.String()
			switch strings.ToLower(strings.TrimSpace(kind)) {
			case "still":
				if programme.StillPath == nil {
					programme.StillPath = &path
				}
			case "backdrop":
				if programme.BackdropPath == nil {
					programme.BackdropPath = &path
				}
			}
		}
		i++
	}
	return programme
}

type rating struct {
	value   *string
	discard bool
}

// ratingValue ports ProcessRating (XmlTvReader.cs:533-592): an empty <value>
// discards the rating; the last value wins.
func ratingValue(element *node) rating {
	var result rating
	for _, child := range element.children {
		if !strings.EqualFold(child.name, "value") {
			continue
		}
		text := child.text.String()
		if text == "" {
			return rating{discard: true}
		}
		result.value = &text
	}
	return result
}

// parseEpisodeNum ports ProcessEpisodeNum (XmlTvReader.cs:594-635) for the
// fields the provider maps: xmltv_ns, SxxExx and dd_progid. Systems are
// matched with exact case; onscreen is ignored, as upstream ignores it.
func parseEpisodeNum(element *node, programme *XMLTVProgramme) {
	system, _ := attribute(element.attrs, "system")
	value := element.text.String()
	switch system {
	case "dd_progid":
		if strings.TrimSpace(value) != "" {
			programme.ProgramID = value
		}
	case "xmltv_ns":
		components := strings.Split(strings.ReplaceAll(value, " ", ""), ".")
		if n, ok := xmltvNSComponent(components, 0); ok {
			programme.Series = &n
		}
		if n, ok := xmltvNSComponent(components, 1); ok {
			programme.Episode = &n
		}
	case "SxxExx":
		if match := sxxexx.FindStringSubmatch(value); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil {
				programme.Series = &n
			}
			if n, err := strconv.Atoi(match[2]); err == nil {
				programme.Episode = &n
			}
		}
	}
}

// xmltvNSComponent returns component k ("5" or "5/12") plus one, for the
// zero-based xmltv_ns numbering (XmlTvReader.cs:760-826).
func xmltvNSComponent(components []string, k int) (int, bool) {
	if k >= len(components) || components[k] == "" {
		return 0, false
	}
	first, _, _ := strings.Cut(components[k], "/")
	n, err := strconv.Atoi(first)
	if err != nil {
		return 0, false
	}
	return n + 1, true
}
