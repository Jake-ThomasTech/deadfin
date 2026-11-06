// SPDX-License-Identifier: GPL-2.0-only

package catalog

import (
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
)

// ChannelInfo is the channel a tuner reports after the guide metadata is
// merged in: the members of MediaBrowser.Controller/LiveTv/ChannelInfo.cs
// that GuideManager.GetChannel reads.
type ChannelInfo struct {
	// ID is the tuner's external channel id (for M3U, "m3u_…").
	ID          string
	Name        string
	Number      string
	ChannelType item.ChannelType
	// ImagePath is a local image; it wins over ImageURL.
	ImagePath string
	ImageURL  string
	// Tags replaces the stored tags when it is not nil.
	Tags []string
}

// Channel returns the stored form of a tuner channel under the Live TV
// folder. existing is the stored channel, if any; its other members stay.
// It ports GuideManager.GetChannel (GuideManager.cs:381-468) and
// LiveTvChannelImageHelper.UpdateChannelImageIfNeeded.
func Channel(info ChannelInfo, folderID dto.UUID, existing *item.Item, now time.Time) item.Item {
	var channel item.Item
	if existing != nil && existing.Kind == item.KindLiveTvChannel {
		channel = *existing
	} else {
		created := now.UTC()
		channel = item.Item{
			ID:          ChannelID(info.ID),
			Kind:        item.KindLiveTvChannel,
			DateCreated: &created,
		}
	}
	channel.SourceType = item.SourceTypeLiveTV
	channel.PresentationUniqueKey = presentationKey(channel.ID)
	if info.Tags != nil {
		channel.Values.Tags = append([]string(nil), info.Tags...)
	}
	channel.ParentID = ptr(folderID)

	liveTV := item.LiveTVExtra{ChannelType: info.ChannelType, Number: ptr(info.Number)}
	if info.Number == "" {
		liveTV.Number = nil
	}
	channel.Extra.LiveTV = &liveTV

	channel.ProviderIDs.Set(ExternalServiceProvider, ServiceName)
	channel.ExternalID = ptr(info.ID)
	channel.Name = stringOrNil(info.Name)

	source := info.ImagePath
	if strings.TrimSpace(source) == "" {
		source = info.ImageURL
	}
	if strings.TrimSpace(source) != "" {
		channel.ImageInfos = setImage(channel.ImageInfos, item.ImageTypePrimary, source)
	}
	return withSortName(channel)
}

// ChannelFlags are the programme classes GuideManager collects for a
// channel while it stores the channel's programmes
// (GuideManager.cs:263-311).
type ChannelFlags struct {
	IsMovie  bool
	IsSeries bool
	IsSports bool
	IsNews   bool
	IsKids   bool
}

// Add records one programme's classes.
func (f *ChannelFlags) Add(program item.Item) {
	f.IsMovie = f.IsMovie || program.IsMovie
	f.IsSeries = f.IsSeries || program.IsSeries
	for _, tag := range program.Values.Tags {
		switch tag {
		case "Sports":
			f.IsSports = true
		case "News":
			f.IsNews = true
		case "Kids":
			f.IsKids = true
		}
	}
}

// WithFlags applies the classes to a channel. LiveTvChannel.IsNews and
// IsSports have no stored column upstream, so only IsMovie, IsSeries and a
// "Kids" tag persist; the tag, once added, stays (BaseItem.AddTag).
func WithFlags(channel item.Item, flags ChannelFlags) item.Item {
	channel.IsMovie = flags.IsMovie
	channel.IsSeries = flags.IsSeries
	if flags.IsKids && !containsFold(channel.Values.Tags, "Kids") {
		channel.Values.Tags = append(append([]string(nil), channel.Values.Tags...), "Kids")
	}
	return channel
}

func containsFold(values []string, value string) bool {
	for _, candidate := range values {
		if strings.EqualFold(candidate, value) {
			return true
		}
	}
	return false
}

// setImage sets the image of a type at index 0 to path. An existing image
// with the same path stays as it is, with its size and date.
func setImage(images []item.ImageInfo, imageType item.ImageType, path string) []item.ImageInfo {
	out := make([]item.ImageInfo, 0, len(images)+1)
	replaced := false
	for _, image := range images {
		if image.Type == imageType && image.Index == 0 {
			if strings.EqualFold(image.Path, path) {
				out = append(out, image)
			} else {
				out = append(out, item.ImageInfo{Type: imageType, Path: path})
			}
			replaced = true
			continue
		}
		out = append(out, image)
	}
	if !replaced {
		out = append(out, item.ImageInfo{Type: imageType, Path: path})
	}
	return out
}

// removeImage removes the image of a type at index 0.
func removeImage(images []item.ImageInfo, imageType item.ImageType) []item.ImageInfo {
	out := make([]item.ImageInfo, 0, len(images))
	for _, image := range images {
		if image.Type == imageType && image.Index == 0 {
			continue
		}
		out = append(out, image)
	}
	return out
}
