// SPDX-License-Identifier: GPL-2.0-only

package guide_test

import (
	"fmt"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// The fixtures are synthetic snippets in the forms that IPTV guides use
// (deadfin#730): no <episode-num> or <sub-title>, the episode in the first
// description line, and a superscript New marker after the title.
func TestDescriptionEpisodes(t *testing.T) {
	type want struct {
		name         string
		episodeTitle *string
		overview     *string
		season       *int
		episode      *int
		isSeries     bool
		isRepeat     bool
	}
	for _, tc := range []struct {
		label    string
		children string
		want     want
	}{
		{
			label:    "episode and synopsis",
			children: "<title>Seinfeld</title><desc>S07 E17 The Doll\nGeorge is dismayed.</desc>",
			want:     want{"Seinfeld", strptr("The Doll"), strptr("George is dismayed."), intptr(7), intptr(17), true, false},
		},
		{
			label:    "no space between season and episode",
			children: "<title>Seinfeld</title><desc>S7E17 The Doll\r\nGeorge is dismayed.\nSecond line.</desc>",
			want:     want{"Seinfeld", strptr("The Doll"), strptr("George is dismayed.\nSecond line."), intptr(7), intptr(17), true, false},
		},
		{
			label:    "no synopsis keeps the line as overview",
			children: "<title>The View</title><desc>S30 E30 Hot topics.</desc>",
			want:     want{"The View", strptr("Hot topics."), strptr("S30 E30 Hot topics."), intptr(30), intptr(30), true, false},
		},
		{
			label:    "no episode title",
			children: "<title>Seinfeld</title><desc>S07 E17\nGeorge is dismayed.</desc>",
			want:     want{"Seinfeld", nil, strptr("George is dismayed."), intptr(7), intptr(17), true, false},
		},
		{
			label:    "plain description",
			children: "<title>Evening News</title><desc>Season of storms. S01 E02 is not first.</desc>",
			want:     want{"Evening News", nil, strptr("Season of storms. S01 E02 is not first."), nil, nil, false, false},
		},
		{
			label:    "new marker",
			children: "<title>The Simpsons  ᴺᵉʷ</title><desc>S36 E01 Bart's Birthday\nBart turns eleven.</desc><previously-shown/>",
			want:     want{"The Simpsons", strptr("Bart's Birthday"), strptr("Bart turns eleven."), intptr(36), intptr(1), true, false},
		},
		{
			label:    "repeat without marker",
			children: "<title>The Simpsons</title><desc>S07 E17 The Doll</desc><previously-shown/>",
			want:     want{"The Simpsons", strptr("The Doll"), strptr("S07 E17 The Doll"), intptr(7), intptr(17), true, true},
		},
		{
			label:    "episode-num wins",
			children: `<title>Seinfeld  ᴺᵉʷ</title><desc>S07 E17 The Doll` + "\n" + `Synopsis.</desc><episode-num system="xmltv_ns">1.4.</episode-num>`,
			want:     want{"Seinfeld", nil, strptr("S07 E17 The Doll\nSynopsis."), intptr(2), intptr(5), true, false},
		},
		{
			label:    "sub-title wins",
			children: "<title>Seinfeld</title><sub-title>The Doll</sub-title><desc>S07 E17 Other\nSynopsis.</desc>",
			want:     want{"Seinfeld", strptr("The Doll"), strptr("S07 E17 Other\nSynopsis."), nil, nil, false, false},
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			program := guide.MapProgramme(readOne(t, tc.children, nil), config.DefaultListingsProviderInfo())
			got := want{deref(program.Name), program.EpisodeTitle, program.Overview, program.SeasonNumber, program.EpisodeNumber, program.IsSeries, program.IsRepeat}
			if got.name != tc.want.name || !equalString(got.episodeTitle, tc.want.episodeTitle) || !equalString(got.overview, tc.want.overview) ||
				!equalInt(got.season, tc.want.season) || !equalInt(got.episode, tc.want.episode) || got.isSeries != tc.want.isSeries || got.isRepeat != tc.want.isRepeat {
				t.Fatalf("program = %s\nwant      %s", describe(got.name, got.episodeTitle, got.overview, got.season, got.episode, got.isSeries, got.isRepeat),
					describe(tc.want.name, tc.want.episodeTitle, tc.want.overview, tc.want.season, tc.want.episode, tc.want.isSeries, tc.want.isRepeat))
			}
		})
	}
}

func TestDescriptionEpisodesSeriesID(t *testing.T) {
	info := config.DefaultListingsProviderInfo()
	fresh := guide.MapProgramme(readOne(t, "<title>The Simpsons  ᴺᵉʷ</title><desc>S36 E01 Bart's Birthday</desc>", nil), info)
	repeat := guide.MapProgramme(readOne(t, "<title>The Simpsons</title><desc>S07 E17 The Doll</desc>", nil), info)
	if fresh.SeriesID == nil || repeat.SeriesID == nil || *fresh.SeriesID != *repeat.SeriesID || *fresh.SeriesID != tuner.DotNetMD5("The Simpsons") {
		t.Fatalf("SeriesID new = %v, repeat = %v; want one series without the marker", fresh.SeriesID, repeat.SeriesID)
	}
}

func TestDescriptionEpisodesOff(t *testing.T) {
	info := config.DefaultListingsProviderInfo()
	off := false
	info.ParseDescriptionEpisodes = &off
	program := guide.MapProgramme(readOne(t, "<title>Seinfeld  ᴺᵉʷ</title><desc>S07 E17 The Doll\nGeorge is dismayed.</desc>", nil), info)
	if deref(program.Name) != "Seinfeld  ᴺᵉʷ" || deref(program.Overview) != "S07 E17 The Doll\nGeorge is dismayed." ||
		program.EpisodeTitle != nil || program.SeasonNumber != nil || program.EpisodeNumber != nil || program.IsSeries {
		t.Fatalf("literal program = %+v, want the text unchanged", program)
	}
}

func deref(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func equalString(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func equalInt(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func describe(name string, episodeTitle, overview *string, season, episode *int, isSeries, isRepeat bool) string {
	number := func(n *int) any {
		if n == nil {
			return "<nil>"
		}
		return *n
	}
	return fmt.Sprintf("name=%q episodeTitle=%q overview=%q season=%v episode=%v series=%t repeat=%t",
		name, deref(episodeTitle), deref(overview), number(season), number(episode), isSeries, isRepeat)
}
