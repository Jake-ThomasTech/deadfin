// SPDX-License-Identifier: GPL-2.0-only

package catalog

import (
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
)

// Program returns the stored form of a guide programme under its channel.
// program.ID and program.ChannelID are the final ids that
// ListingsManager.GetProgramsAsync gives (the guide id plus "_" and the
// tuner channel id, and the tuner channel id). existing is the stored
// programme, if any. It ports GuideManager.GetProgram
// (GuideManager.cs:470-655) without the change tracking: the store writes a
// programme only when the result differs from the stored row.
func Program(program guide.Program, channel item.Item, existing *item.Item, now time.Time) item.Item {
	var stored item.Item
	if existing != nil && existing.Kind == item.KindLiveTvProgram {
		stored = *existing
	} else {
		created := now.UTC()
		stored = item.Item{
			ID:           ProgramID(program.ID),
			Kind:         item.KindLiveTvProgram,
			DateCreated:  &created,
			DateModified: &created,
		}
	}
	stored.SourceType = item.SourceTypeLiveTV
	stored.PresentationUniqueKey = presentationKey(stored.ID)
	stored.ShowID = program.ShowID
	stored.ParentID = ptr(channel.ID)
	stored.ProgramAudio = nil
	stored.ChannelID = ptr(channel.ID)
	stored.CommunityRating = program.CommunityRating
	stored.EpisodeTitle = program.EpisodeTitle
	stored.ExternalID = ptr(program.ID)
	if program.SeriesID != nil && strings.TrimSpace(*program.SeriesID) != "" {
		stored.ExternalSeriesID = program.SeriesID
	}

	isSeries := program.IsSeries || (program.EpisodeTitle != nil && *program.EpisodeTitle != "")
	if isSeries {
		stored.SeriesName = program.Name
	}
	tags := make([]string, 0, 8)
	for _, flag := range []struct {
		set bool
		tag string
	}{
		{program.IsLive, "Live"},
		{program.IsPremiere, "Premiere"},
		{program.IsNews, "News"},
		{program.IsSports, "Sports"},
		{program.IsKids, "Kids"},
		{program.IsRepeat, "Repeat"},
		{program.IsMovie, "Movie"},
		{isSeries, "Series"},
	} {
		if flag.set {
			tags = append(tags, flag.tag)
		}
	}
	stored.Values.Tags = tags
	stored.Values.Genres = append([]string(nil), program.Genres...)
	// ProgramInfo.IsHD is always null for XMLTV, so Width and Height stay.

	stored.IsMovie = program.IsMovie
	stored.IsRepeat = program.IsRepeat
	stored.IsSeries = isSeries
	stored.Name = program.Name
	stored.OfficialRating = program.OfficialRating
	stored.Overview = program.Overview
	stored.RunTimeTicks = ptr(int64(program.EndDate.Sub(program.StartDate) / 100))
	start, end := program.StartDate.UTC(), program.EndDate.UTC()
	stored.StartDate = &start
	stored.EndDate = &end
	stored.ProductionYear = program.ProductionYear
	if !isSeries || program.IsRepeat {
		stored.PremiereDate = program.OriginalAirDate
	}
	stored.IndexNumber = program.EpisodeNumber
	stored.ParentIndexNumber = program.SeasonNumber

	// GetProgram replaces ProviderIds with ProgramInfo.ProviderIds (always
	// empty for XMLTV) and then restores the etag (GuideManager.cs:593-637).
	stored.ProviderIDs = item.ProviderIDs{}
	if strings.TrimSpace(program.Etag) != "" {
		stored.ProviderIDs.Set(EtagProvider, program.Etag)
	}

	stored.ImageInfos = programImage(stored.ImageInfos, item.ImageTypePrimary, program.ImageURL)
	stored.ImageInfos = programImage(stored.ImageInfos, item.ImageTypeThumb, program.ThumbImageURL)
	stored.ImageInfos = programImage(stored.ImageInfos, item.ImageTypeLogo, nil)
	stored.ImageInfos = programImage(stored.ImageInfos, item.ImageTypeBackdrop, program.BackdropImageURL)
	return withSortName(stored)
}

// programImage ports GuideManager.UpdateImage (GuideManager.cs:675-723) for
// remote URLs: a blank URL removes the image.
func programImage(images []item.ImageInfo, imageType item.ImageType, url *string) []item.ImageInfo {
	if url == nil || strings.TrimSpace(*url) == "" {
		return removeImage(images, imageType)
	}
	return setImage(images, imageType, *url)
}

// StoredEtag returns the guide etag stored on a programme.
func StoredEtag(program item.Item) string {
	value, _ := program.ProviderIDs.Get(EtagProvider)
	return value
}
