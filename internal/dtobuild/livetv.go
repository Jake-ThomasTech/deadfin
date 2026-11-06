// SPDX-License-Identifier: GPL-2.0-only

package dtobuild

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"github.com/deadfin-org/deadfin/internal/clock"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// WithClock sets the clock that selects a channel's current programme. A
// builder without a clock does not add CurrentProgram.
func (b *Builder) WithClock(clk clock.Clock) *Builder {
	b.clock = clk
	return b
}

// projectLiveTv adds the Live TV facts that DtoService.GetBaseItemDtos asks
// LiveTvManager for: AddInfoToProgramDto for programmes and AddChannelInfo
// for channels (LiveTvManager.cs:540-580, 959-1015).
func (b *Builder) projectLiveTv(ctx context.Context, values []item.Item, results []dto.BaseItemDto, options store.ItemQueryDTOOptions, userID *dto.UUID) error {
	var programs, channels []int
	for index, value := range values {
		switch value.Kind {
		case item.KindLiveTvProgram:
			programs = append(programs, index)
		case item.KindLiveTvChannel:
			channels = append(channels, index)
		default:
			continue
		}
		results[index].Type = dto.Kind(item.ClientTypeName(value))
	}
	if len(programs) != 0 {
		if err := b.projectPrograms(ctx, values, results, programs, options); err != nil {
			return err
		}
	}
	if len(channels) != 0 {
		return b.projectChannels(ctx, values, results, channels, options, userID)
	}
	return nil
}

// projectPrograms ports AddInfoToProgramDto. The DTO flags are bool? and the
// source ORs a bool into them, so a false flag stays null.
func (b *Builder) projectPrograms(ctx context.Context, values []item.Item, results []dto.BaseItemDto, indexes []int, options store.ItemQueryDTOOptions) error {
	hasChannelInfo := hasField(options.Fields, "ChannelInfo")
	hasChannelImage := hasField(options.Fields, "ChannelImage")
	for _, index := range indexes {
		value, result := values[index], &results[index]
		if value.StartDate != nil {
			start := *value.StartDate
			result.StartDate = &start
		}
		result.EpisodeTitle = dereference(value.EpisodeTitle)
		result.IsRepeat = trueOrNil(value.IsRepeat)
		result.IsMovie = trueOrNil(value.IsMovie)
		result.IsSeries = trueOrNil(value.IsSeries)
		result.IsSports = trueOrNil(hasTag(value, "Sports"))
		result.IsLive = trueOrNil(hasTag(value, "Live"))
		result.IsNews = trueOrNil(hasTag(value, "News"))
		result.IsKids = trueOrNil(hasTag(value, "Kids"))
		result.IsPremiere = trueOrNil(hasTag(value, "Premiere"))
	}
	if (!hasChannelInfo && !hasChannelImage) || b.items == nil {
		return nil
	}
	ids := make([]dto.UUID, 0, len(indexes))
	seen := make(map[dto.UUID]struct{}, len(indexes))
	for _, index := range indexes {
		if id := values[index].ChannelID; id != nil {
			if _, ok := seen[*id]; !ok {
				seen[*id] = struct{}{}
				ids = append(ids, *id)
			}
		}
	}
	loaded, err := b.items.GetBatch(ctx, ids)
	if err != nil {
		return err
	}
	byID := make(map[dto.UUID]item.Item, len(loaded))
	for _, channel := range loaded {
		if channel.Kind == item.KindLiveTvChannel {
			byID[channel.ID] = channel
		}
	}
	for _, index := range indexes {
		if values[index].ChannelID == nil {
			continue
		}
		channel, ok := byID[*values[index].ChannelID]
		if !ok {
			continue
		}
		result := &results[index]
		result.ChannelName = dereference(channel.Name)
		result.MediaType = dto.MediaType(item.MediaTypeOf(channel).String())
		result.ChannelNumber = channelNumber(channel)
		if hasChannelImage {
			result.ChannelPrimaryImageTag = primaryImageTag(channel)
		}
	}
	return nil
}

// projectChannels ports AddChannelInfo. The current programme query uses
// the source's bounds, order and limit of one row per channel.
func (b *Builder) projectChannels(ctx context.Context, values []item.Item, results []dto.BaseItemDto, indexes []int, options store.ItemQueryDTOOptions, userID *dto.UUID) error {
	for _, index := range indexes {
		value, result := values[index], &results[index]
		result.Number = channelNumber(value)
		result.ChannelNumber = result.Number
		channelType := item.ChannelTypeTV
		if value.Extra.LiveTV != nil {
			channelType = value.Extra.LiveTV.ChannelType
		}
		projected := dto.ChannelType(channelType.String())
		result.ChannelType = &projected
	}
	if !options.AddCurrentProgram || b.items == nil || b.clock == nil {
		return nil
	}
	ids := make([]dto.UUID, 0, len(indexes))
	seen := make(map[dto.UUID]struct{}, len(indexes))
	// Programmes are stored under their channel's folder, so the folders
	// scope the query to the guide index (ix_items_top_kind_channel_start).
	var folders []dto.UUID
	scoped := true
	for _, index := range indexes {
		value := values[index]
		if _, ok := seen[value.ID]; !ok {
			seen[value.ID] = struct{}{}
			ids = append(ids, value.ID)
		}
		if value.ParentID == nil {
			scoped = false
		} else if !slices.Contains(folders, *value.ParentID) {
			folders = append(folders, *value.ParentID)
		}
	}
	if !scoped {
		folders = nil
	}
	now := b.clock.Now().UTC()
	limit := len(ids)
	found, err := b.items.Query(ctx, store.ItemQuery{
		IncludeItemTypes: []item.Kind{item.KindLiveTvProgram},
		ChannelIDs:       ids,
		TopParentIDs:     folders,
		MaxStartDate:     &now,
		MinEndDate:       &now,
		Limit:            &limit,
		OrderBy:          []store.ItemOrder{{SortBy: store.ItemSortByStartDate, SortOrder: store.SortOrderAscending}},
		UserID:           userID,
	})
	if err != nil {
		return err
	}
	// The first programme per channel wins, as programs.FirstOrDefault.
	current := make([]item.Item, 0, len(found.Items))
	taken := make(map[dto.UUID]struct{}, len(found.Items))
	for _, program := range found.Items {
		if program.ChannelID == nil {
			continue
		}
		if _, ok := taken[*program.ChannelID]; ok {
			continue
		}
		taken[*program.ChannelID] = struct{}{}
		current = append(current, program)
	}
	if len(current) == 0 {
		return nil
	}
	page, err := b.Build(ctx, current, withoutChannelRemovedFields(options), userID)
	if err != nil {
		return err
	}
	byChannel := make(map[dto.UUID]*dto.BaseItemDto, len(page.Items))
	for index := range page.Items {
		if channelID := page.Items[index].ChannelID; channelID != nil {
			byChannel[*channelID] = &page.Items[index]
		}
	}
	for _, index := range indexes {
		if program, ok := byChannel[values[index].ID]; ok {
			results[index].CurrentProgram = program
		}
	}
	return nil
}

// withoutChannelRemovedFields ports LiveTvManager.RemoveFields, which
// AddChannelInfo applies before it builds the current programmes.
func withoutChannelRemovedFields(options store.ItemQueryDTOOptions) store.ItemQueryDTOOptions {
	out := options
	out.Fields = make([]string, 0, len(options.Fields))
	for _, field := range options.Fields {
		switch field {
		case "CanDelete", "CanDownload", "DisplayPreferencesId", "Etag":
			continue
		}
		out.Fields = append(out.Fields, field)
	}
	return out
}

func channelNumber(channel item.Item) string {
	if channel.Extra.LiveTV == nil {
		return ""
	}
	return dereference(channel.Extra.LiveTV.Number)
}

// primaryImageTag returns the tag of the item's Primary image, as the
// enricher computes ImageTags.
func primaryImageTag(value item.Item) string {
	for _, image := range value.ImageInfos {
		if image.Type != item.ImageTypePrimary || image.Index != 0 {
			continue
		}
		if tag := filepath.Base(image.Path); tag != "" && tag != "." {
			return tag
		}
	}
	return ""
}

// hasTag ports the LiveTvProgram IsSports, IsLive, IsNews, IsKids and
// IsPremiere properties: an ordinal case-insensitive tag match.
func hasTag(value item.Item, tag string) bool {
	for _, candidate := range value.Values.Tags {
		if strings.EqualFold(candidate, tag) {
			return true
		}
	}
	return false
}

func trueOrNil(value bool) *bool {
	if !value {
		return nil
	}
	return boolPointer(true)
}
