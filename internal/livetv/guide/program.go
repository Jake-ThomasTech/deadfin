// SPDX-License-Identifier: GPL-2.0-only

// Package guide reads XMLTV listings and matches guide channels to tuner
// channels. It ports src/Jellyfin.LiveTv/Listings (XmlTvListingsProvider,
// ListingsManager, EpgChannelData, XmlTvProgramEtag) and the XmlTvReader of
// the Jellyfin.XmlTv package that upstream pins.
//
// Upstream re-opens and re-scans the XMLTV file once per channel
// (XmlTvListingsProvider.cs:162-177). This reader scans it once per refresh
// and keeps only the programmes of the wanted channels.
package guide

import "time"

// Program is one guide programme: the fields of
// MediaBrowser.Controller/LiveTv/ProgramInfo.cs that the XMLTV provider
// fills. Nullable C# members are pointers.
type Program struct {
	ID               string
	ChannelID        string
	Name             *string
	OfficialRating   *string
	Overview         *string
	StartDate        time.Time
	EndDate          time.Time
	Genres           []string
	OriginalAirDate  *time.Time
	CommunityRating  *float32
	IsRepeat         bool
	EpisodeTitle     *string
	ImageURL         *string
	ThumbImageURL    *string
	BackdropImageURL *string
	HasImage         bool
	IsMovie          bool
	IsSports         bool
	IsSeries         bool
	IsLive           bool
	IsNews           bool
	IsKids           bool
	IsPremiere       bool
	ProductionYear   *int
	SeriesID         *string
	ShowID           *string
	SeasonNumber     *int
	EpisodeNumber    *int
	Etag             string
}
