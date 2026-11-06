// SPDX-License-Identifier: GPL-2.0-only

package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the tz and DST quirks must not depend on host zoneinfo
)

// XMLTV document types. Only the elements upstream's XmlTvListingsProvider
// reads are produced: channel display-name and icon; programme title,
// sub-title, desc, category, episode-num (xmltv_ns and onscreen), new,
// previously-shown, date, icon.
type xmltvChannel struct {
	XMLName     xml.Name   `xml:"channel"`
	ID          string     `xml:"id,attr"`
	DisplayName []string   `xml:"display-name"`
	Icon        *xmltvIcon `xml:"icon,omitempty"`
}

type xmltvIcon struct {
	Src string `xml:"src,attr"`
}

type xmltvEpisodeNum struct {
	System string `xml:"system,attr"`
	Value  string `xml:",chardata"`
}

type xmltvProgramme struct {
	XMLName         xml.Name          `xml:"programme"`
	Start           string            `xml:"start,attr"`
	Stop            string            `xml:"stop,attr,omitempty"`
	Channel         string            `xml:"channel,attr"`
	Title           string            `xml:"title"`
	SubTitle        string            `xml:"sub-title,omitempty"`
	Desc            string            `xml:"desc,omitempty"`
	Date            string            `xml:"date,omitempty"`
	Category        []string          `xml:"category"`
	EpisodeNum      []xmltvEpisodeNum `xml:"episode-num"`
	New             *struct{}         `xml:"new,omitempty"`
	PreviouslyShown *struct{}         `xml:"previously-shown,omitempty"`
	Icon            *xmltvIcon        `xml:"icon,omitempty"`
}

type showTemplate struct {
	title    string
	category []string
	minutes  int
	series   bool
}

// Each channel cycles through a fixed schedule so a test can predict the
// programme at any instant from the anchor time alone.
var showsByGroup = map[string][]showTemplate{
	"Broadcast": {
		{"Fake Morning Show", []string{"Talk"}, 60, true},
		{"Fake Sitcom", []string{"Comedy", "Series"}, 30, true},
		{"Fake Drama", []string{"Drama", "Series"}, 60, true},
		{"Fake Movie Night", []string{"Movie"}, 120, false},
	},
	"Cable": {
		{"Fake Cooking", []string{"Cooking", "Series"}, 30, true},
		{"Fake Documentary", []string{"Documentary"}, 60, true},
	},
	"News": {
		{"Fake News Hour", []string{"News"}, 60, true},
	},
	"Sports": {
		{"Fake Football", []string{"Sports", "Football"}, 180, false},
		{"Fake Sports Center", []string{"Sports", "News"}, 60, true},
	},
	"Kids": {
		{"Fake Cartoons", []string{"Kids", "Animation"}, 30, true},
		{"Fake Puppets", []string{"Children"}, 15, true},
	},
	"Movies": {
		{"Fake Feature", []string{"Movie"}, 105, false},
		{"Fake Short", []string{"Movie", "Short"}, 20, false},
	},
	"Faults": {
		{"Fault Test", []string{"Test"}, 30, true},
	},
}

const xmltvTime = "20060102150405 -0700"

// quirks is the set of enabled guide and lineup quirks.
type quirks map[string]bool

// allQuirks is what -quirks all enables. Each one is a defect seen in real
// guides and playlists.
var allQuirks = []string{
	"missing-ids",  // M3U entries with no tvg-id (match by name/number only)
	"dupes",        // the same channel twice in M3U and XMLTV, with duplicate programmes
	"tz",           // per-channel UTC offsets: +0000, -0400/-0500, +0530, +1245/+1345
	"overlap",      // a programme that starts before the previous one stops
	"gaps",         // holes between programmes
	"no-stop",      // programmes without a stop attribute
	"entities",     // &, quotes, angle brackets, long titles, non-ASCII and emoji
	"reverse",      // programmes in reverse time order within a channel
	"xtream-types", // player_api: ids as string or number, empty epg_channel_id, null icons
}

func parseQuirks(s string) (quirks, error) {
	q := quirks{}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "" || name == "none":
		case name == "all":
			for _, a := range allQuirks {
				q[a] = true
			}
		case slices.Contains(allQuirks, name):
			q[name] = true
		default:
			return nil, fmt.Errorf("unknown quirk %q (have %s)", name, strings.Join(allQuirks, ","))
		}
	}
	return q, nil
}

func (q quirks) has(name string) bool { return q[name] }

var tzZones = func() []*time.Location {
	names := []string{"UTC", "America/New_York", "Asia/Kolkata", "Pacific/Chatham"}
	out := make([]*time.Location, len(names))
	for i, n := range names {
		loc, err := time.LoadLocation(n)
		if err != nil {
			panic(err) // embedded tzdata; cannot fail
		}
		out[i] = loc
	}
	return out
}()

// writeGuide streams an XMLTV guide covering [anchor, anchor+days) to w. It
// never holds more than one channel's programmes, so 20k channels x 14 days
// (several GB) is generated in constant memory. It returns the programme
// count.
func writeGuide(w io.Writer, base string, channels []Channel, anchor time.Time, days int, q quirks) (int, error) {
	anchor = anchor.UTC().Truncate(time.Hour)
	end := anchor.Add(time.Duration(days) * 24 * time.Hour)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return 0, err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", " ")
	tv := xml.StartElement{Name: xml.Name{Local: "tv"}, Attr: []xml.Attr{{Name: xml.Name{Local: "generator-info-name"}, Value: "deadfin-fakehdhr"}}}
	if err := enc.EncodeToken(tv); err != nil {
		return 0, err
	}
	for i, c := range channels {
		if c.NoID {
			continue
		}
		names := []string{c.Name, c.Number}
		if c.CallSign != "" {
			names = append(names, c.CallSign)
		}
		ch := xmltvChannel{ID: c.guideID(), DisplayName: names, Icon: &xmltvIcon{Src: base + "/logo/" + c.Number + ".svg"}}
		if err := enc.Encode(ch); err != nil {
			return 0, err
		}
		if q.has("dupes") && i%50 == 49 {
			if err := enc.Encode(ch); err != nil {
				return 0, err
			}
		}
	}
	count := 0
	for i, c := range channels {
		if c.NoGuide || c.NoID {
			continue
		}
		idx := i
		if c.Sched > 0 {
			idx = c.Sched - 1
		}
		progs := channelProgrammes(idx, c, base, anchor, end, q)
		if q.has("dupes") && i%50 == 49 {
			progs = append(progs, progs...)
		}
		for _, p := range progs {
			if err := enc.Encode(p); err != nil {
				return count, err
			}
			count++
		}
		if err := enc.Flush(); err != nil {
			return count, err
		}
	}
	if err := enc.EncodeToken(tv.End()); err != nil {
		return count, err
	}
	return count, enc.Flush()
}

func channelProgrammes(index int, c Channel, base string, anchor, end time.Time, q quirks) []xmltvProgramme {
	shows := showsByGroup[c.Group]
	if len(shows) == 0 {
		return nil
	}
	loc := time.UTC
	if q.has("tz") {
		loc = tzZones[index%len(tzZones)]
	}
	var out []xmltvProgramme
	episode := map[string]int{}
	t := anchor.Add(c.Shift)
	for k := 0; t.Before(end); k++ {
		s := shows[(index+k)%len(shows)]
		start := t
		if q.has("overlap") && k%7 == 6 {
			start = t.Add(-5 * time.Minute)
		}
		if q.has("gaps") && k%11 == 10 {
			start = t.Add(10 * time.Minute)
		}
		stop := start.Add(time.Duration(s.minutes) * time.Minute)
		p := xmltvProgramme{
			Start:    start.In(loc).Format(xmltvTime),
			Stop:     stop.In(loc).Format(xmltvTime),
			Channel:  c.guideID(),
			Title:    s.title,
			Desc:     fmt.Sprintf("%s on %s (channel %s).", s.title, c.Name, c.Number),
			Category: s.category,
			Icon:     &xmltvIcon{Src: base + "/logo/" + c.Number + ".svg"},
		}
		if q.has("no-stop") && k%13 == 12 {
			p.Stop = ""
		}
		if q.has("entities") {
			switch {
			case k%5 == 0:
				p.Title = s.title + ` & "Friends" <Live>`
			case k%9 == 0:
				p.Title = s.title + ": " + strings.Repeat("Very Long Title ", 20)
			case k%17 == 0:
				p.Title = s.title + " — Café Ñandú 📺"
			}
		}
		if s.series {
			episode[s.title]++
			n := episode[s.title]
			season, ep := 1+(n-1)/20, 1+(n-1)%20
			p.SubTitle = fmt.Sprintf("Episode %d", n)
			p.EpisodeNum = []xmltvEpisodeNum{
				{System: "xmltv_ns", Value: fmt.Sprintf("%d.%d.", season-1, ep-1)},
				{System: "onscreen", Value: fmt.Sprintf("S%02dE%02d", season, ep)},
			}
			// Every third airing is a rerun, so RecordNewOnly has work to do.
			if n%3 == 0 {
				p.PreviouslyShown = &struct{}{}
			} else {
				p.New = &struct{}{}
			}
		} else {
			p.Date = start.Format("2006")
		}
		out = append(out, p)
		t = stop
	}
	if q.has("reverse") {
		slices.Reverse(out)
	}
	return out
}
