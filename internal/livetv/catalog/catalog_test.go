// SPDX-License-Identifier: GPL-2.0-only

package catalog_test

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
)

var now = time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

func strptr(value string) *string { return &value }

func intptr(value int) *int { return &value }

// TestFolderIDMatchesLibrary checks the local GetNewItemId port against
// library.ItemID, which the scanner uses for every library item.
func TestFolderIDMatchesLibrary(t *testing.T) {
	for _, tc := range []struct{ metadata, programData string }{
		{"/var/lib/deadfin/metadata", "/var/lib/deadfin"},
		{"/srv/Metadata", "/var/lib/deadfin"},
	} {
		folder := catalog.Folder(tc.metadata, tc.programData, now)
		key := filepath.Join(tc.metadata, "views", "livetv") + "_namedview_" + catalog.FolderName
		if want := library.ItemID(item.KindUserView, key, tc.programData); folder.ID != want {
			t.Errorf("%s: folder id %s, want %s", tc.metadata, folder.ID, want)
		}
	}
}

// TestIDs pins the ids of LiveTvDtoService (GetInternalChannelId,
// GetInternalProgramId): GetNewItemId(lower("Emby" + id + "4"), type). The
// key is lower-cased, so the external id's case does not matter.
func TestIDs(t *testing.T) {
	if catalog.ChannelID("m3u_ABC") != catalog.ChannelID("m3u_abc") {
		t.Error("channel id depends on case")
	}
	if catalog.ChannelID("x") == catalog.ProgramID("x") {
		t.Error("channel and programme ids collide")
	}
	// md5(UTF-16LE(type + key)) in .NET Guid byte order, computed with
	// Python hashlib.
	if got, want := catalog.ChannelID("m3u_a").String(), "1062f0f6cc4f3d4c7a0dd1cfe8605836"; got != want {
		t.Errorf("channel id %s, want %s", got, want)
	}
	if got, want := catalog.ProgramID("x").String(), "7ef2f63681d3878fc3a3613139490af1"; got != want {
		t.Errorf("programme id %s, want %s", got, want)
	}
}

func TestChannel(t *testing.T) {
	folder := dto.UUID{1}
	info := catalog.ChannelInfo{ID: "m3u_a", Name: "A", Number: "2.1", ImageURL: "https://logo/a.png", ChannelType: item.ChannelTypeRadio}
	channel := catalog.Channel(info, folder, nil, now)
	if channel.ID != catalog.ChannelID("m3u_a") || channel.Kind != item.KindLiveTvChannel || *channel.ParentID != folder {
		t.Fatalf("%+v", channel)
	}
	if *channel.Name != "A" || *channel.ExternalID != "m3u_a" || *channel.Extra.LiveTV.Number != "2.1" || channel.Extra.LiveTV.ChannelType != item.ChannelTypeRadio {
		t.Errorf("fields %+v", channel)
	}
	if value, _ := channel.ProviderIDs.Get("ExternalServiceId"); value != "Emby" {
		t.Errorf("service %q", value)
	}
	if channel.SourceType != item.SourceTypeLiveTV || *channel.PresentationUniqueKey != channel.ID.String() || !channel.DateCreated.Equal(now) {
		t.Errorf("source %v key %v created %v", channel.SourceType, channel.PresentationUniqueKey, channel.DateCreated)
	}
	if !reflect.DeepEqual(channel.ImageInfos, []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "https://logo/a.png"}}) {
		t.Errorf("images %+v", channel.ImageInfos)
	}

	// A refresh keeps the stored image record and the creation date; a
	// local image path wins over the URL; nil tags keep the stored tags.
	stored := channel
	stored.ImageInfos[0].Width = 100
	stored.Values.Tags = []string{"Kids"}
	later := catalog.Channel(info, folder, &stored, now.Add(time.Hour))
	if later.ImageInfos[0].Width != 100 || !later.DateCreated.Equal(now) || !reflect.DeepEqual(later.Values.Tags, []string{"Kids"}) {
		t.Errorf("refresh %+v", later)
	}
	info.ImagePath = "/logos/a.png"
	info.Tags = []string{}
	if later := catalog.Channel(info, folder, &stored, now); later.ImageInfos[0].Path != "/logos/a.png" || len(later.Values.Tags) != 0 {
		t.Errorf("image %+v tags %q", later.ImageInfos, later.Values.Tags)
	}
}

func TestWithFlags(t *testing.T) {
	var flags catalog.ChannelFlags
	flags.Add(item.Item{IsSeries: true, Values: item.ItemValues{Tags: []string{"Kids", "Sports"}}})
	flags.Add(item.Item{IsMovie: true})
	channel := catalog.WithFlags(item.Item{Values: item.ItemValues{Tags: []string{"kids"}}}, flags)
	if !channel.IsMovie || !channel.IsSeries || !reflect.DeepEqual(channel.Values.Tags, []string{"kids"}) || !flags.IsSports {
		t.Errorf("%+v %+v", channel, flags)
	}
	if channel := catalog.WithFlags(item.Item{}, flags); !reflect.DeepEqual(channel.Values.Tags, []string{"Kids"}) {
		t.Errorf("tags %q", channel.Values.Tags)
	}
}

func TestProgram(t *testing.T) {
	channel := item.Item{ID: catalog.ChannelID("m3u_a")}
	start := time.Date(2026, 3, 8, 13, 0, 0, 0, time.FixedZone("", -5*3600))
	p := guide.Program{
		ID:               "fake_2026-03-08T13:00:00.0000000-05:00_m3u_a",
		ChannelID:        "m3u_a",
		Name:             strptr("Show"),
		EpisodeTitle:     strptr("Pilot"),
		StartDate:        start,
		EndDate:          start.Add(90 * time.Minute),
		Genres:           []string{"news", "drama"},
		IsNews:           true,
		IsLive:           true,
		IsRepeat:         true,
		ImageURL:         strptr("https://img/p.png"),
		BackdropImageURL: strptr("https://img/b.png"),
		SeriesID:         strptr("series"),
		ShowID:           strptr("show"),
		SeasonNumber:     intptr(1),
		EpisodeNumber:    intptr(2),
		ProductionYear:   intptr(2020),
		Etag:             "xmltv-sha256-v1:ABC",
	}
	got := catalog.Program(p, channel, nil, now)
	if got.ID != catalog.ProgramID(p.ID) || got.Kind != item.KindLiveTvProgram || *got.ParentID != channel.ID || *got.ChannelID != channel.ID {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(got.Values.Tags, []string{"Live", "News", "Repeat", "Series"}) || !reflect.DeepEqual(got.Values.Genres, p.Genres) {
		t.Errorf("tags %q genres %q", got.Values.Tags, got.Values.Genres)
	}
	if !got.IsSeries || !got.IsRepeat || *got.SeriesName != "Show" || *got.ExternalSeriesID != "series" || *got.ShowID != "show" {
		t.Errorf("series fields %+v", got)
	}
	if *got.RunTimeTicks != int64(90*time.Minute/100) || !got.StartDate.Equal(start) || got.StartDate.Location() != time.UTC {
		t.Errorf("runtime %d start %v", *got.RunTimeTicks, got.StartDate)
	}
	if *got.IndexNumber != 2 || *got.ParentIndexNumber != 1 || *got.ProductionYear != 2020 || *got.ExternalID != p.ID {
		t.Errorf("numbers %+v", got)
	}
	if catalog.StoredEtag(got) != p.Etag {
		t.Errorf("etag %q", catalog.StoredEtag(got))
	}
	wantImages := []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "https://img/p.png"}, {Type: item.ImageTypeBackdrop, Path: "https://img/b.png"}}
	if !reflect.DeepEqual(got.ImageInfos, wantImages) {
		t.Errorf("images %+v", got.ImageInfos)
	}

	// A refresh with no series id keeps the stored one, drops removed
	// images and keeps DateCreated.
	p.SeriesID = nil
	p.BackdropImageURL = nil
	p.Etag = ""
	later := catalog.Program(p, channel, &got, now.Add(time.Hour))
	if *later.ExternalSeriesID != "series" || len(later.ImageInfos) != 1 || !later.DateCreated.Equal(now) || catalog.StoredEtag(later) != "" {
		t.Errorf("refresh %+v", later)
	}

	// An episode title alone makes a series programme; a movie is not one.
	movie := guide.Program{ID: "m", ChannelID: "m3u_a", Name: strptr("Film"), StartDate: start, EndDate: start.Add(time.Hour), IsMovie: true}
	if got := catalog.Program(movie, channel, nil, now); got.IsSeries || got.SeriesName != nil || !reflect.DeepEqual(got.Values.Tags, []string{"Movie"}) {
		t.Errorf("movie %+v", got)
	}
}
