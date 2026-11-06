// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv/catalog"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// RefreshResult describes one guide refresh.
type RefreshResult struct {
	// Channels is the number of tuner channels.
	Channels int
	// Programs is the number of programmes stored for them.
	Programs int
	// Kept is true when a tuner or guide read failed, so the refresh kept
	// stored channels or programmes it could not read again.
	Kept bool
	Changes
}

// channelWriteChunk is the number of channels one write transaction
// stores.
const channelWriteChunk = 500

// channelState follows one tuner channel through the refresh.
type channelState struct {
	index   int
	tuner   tuner.Channel
	item    item.Item
	existed bool
	// count is the number of programmes read for the channel; they wait
	// in the refresh's programSpill until the channel's batch is written.
	count int
	// programs are the channel's programmes while its batch is written.
	programs []guide.Program
	// done is set when a provider gave programmes; later providers skip
	// the channel (ListingsManager.GetProgramsAsync returns the first
	// non-empty list).
	done bool
	// failed is set when the guide read for the channel failed; its stored
	// programmes stay.
	failed bool
}

// RefreshGuide stores the tuner channels and their guide programmes. It
// ports GuideManager.RefreshGuide and RefreshChannelsInternal
// (GuideManager.cs:101-311) for the built-in service.
//
// A failed tuner or guide read keeps the stored channels or programmes it
// would replace; upstream deletes them at the end of the refresh (D-76).
// progress receives 0 to 100; it may be nil.
func (s *Service) RefreshGuide(ctx context.Context, progress func(float64)) (RefreshResult, error) {
	s.refresh.Lock()
	defer s.refresh.Unlock()
	report := func(value float64) {
		if progress != nil {
			progress(value)
		}
	}
	var result RefreshResult
	now := s.options.Clock.Now().UTC()
	options := s.options.Config.LiveTv()

	folder := catalog.Folder(s.options.Paths.InternalMetadata, s.options.Paths.ProgramData, now)
	stored, folderExists, err := s.options.Items.Item(ctx, folder.ID)
	if err != nil {
		return result, fmt.Errorf("livetv: read folder: %w", err)
	}
	if !folderExists && len(options.TunerHosts) == 0 {
		report(100)
		return result, nil
	}
	report(10)

	tunerChannels, tunersKept := s.tunerChannels(ctx, options)
	providers := s.listings(options)
	epg := s.addProviderMetadata(ctx, providers, tunerChannels)

	var existing map[dto.UUID]item.Item
	if folderExists {
		folder = stored
		children, err := s.options.Items.Children(ctx, folder.ID)
		if err != nil {
			return result, fmt.Errorf("livetv: read channels: %w", err)
		}
		existing = byID(children)
	}
	states := make([]*channelState, 0, len(tunerChannels))
	for _, channel := range tunerChannels {
		state := &channelState{index: len(states), tuner: channel}
		var previous *item.Item
		if value, ok := existing[catalog.ChannelID(channel.ID)]; ok {
			previous, state.existed = &value, true
		}
		state.item = catalog.Channel(channelInfo(channel), folder.ID, previous, now)
		states = append(states, state)
	}
	report(15)

	start, end := guide.RefreshWindow(now, options.GuideDays)
	s.options.Logger.InfoContext(ctx, "refreshing guide", "days", guide.Days(options.GuideDays), "channels", len(states))
	spill, err := newProgramSpill(s.options.Paths.Cache, len(states), guide.Days(options.GuideDays))
	if err != nil {
		return result, err
	}
	defer spill.close()
	for _, configured := range providers {
		if err := s.readPrograms(ctx, configured, epg[configured.info.Id], states, start, end, spill); err != nil {
			return result, err
		}
	}

	// Collect the channel classes in a first pass over the spilled
	// programmes, so each channel is written once, before its programmes.
	flags := make([]catalog.ChannelFlags, len(states))
	for b := range spill.batches() {
		err := spill.read(b, func(index int, program guide.Program) {
			if state := states[index]; !state.failed {
				flags[index].Add(catalog.Program(program, state.item, nil, now))
			}
		})
		if err != nil {
			return result, err
		}
	}
	for _, state := range states {
		if state.failed {
			result.Kept = true
			continue
		}
		state.item = catalog.WithFlags(state.item, flags[state.index])
	}

	if !folderExists {
		if err := s.options.Items.Upsert(ctx, folder); err != nil {
			return result, fmt.Errorf("livetv: write folder: %w", err)
		}
	}
	// Write the channels in chunks, so no one transaction holds the
	// writer for long. A full replacement, which deletes the channels the
	// tuners no longer list, runs only when there are such channels.
	desired := make([]item.Item, 0, len(states))
	planned := make(map[dto.UUID]struct{}, len(states))
	for _, state := range states {
		desired = append(desired, state.item)
		planned[state.item.ID] = struct{}{}
	}
	for first := 0; first < len(desired); first += channelWriteChunk {
		changes, err := s.options.Items.MergeChildren(ctx, folder.ID, desired[first:min(first+channelWriteChunk, len(desired))])
		if err != nil {
			return result, fmt.Errorf("livetv: write channels: %w", err)
		}
		result.Changes.add(changes)
	}
	stale := false
	for id := range existing {
		if _, ok := planned[id]; !ok {
			stale = true
			break
		}
	}
	if tunersKept {
		// The channels of a failed tuner stay.
		result.Kept = true
	} else if stale {
		changes, err := s.options.Items.ReplaceChildren(ctx, folder.ID, desired)
		if err != nil {
			return result, fmt.Errorf("livetv: write channels: %w", err)
		}
		result.Changes.add(changes)
	}
	result.Channels = len(states)

	// Write the programmes one batch of channels at a time.
	for b := range spill.batches() {
		err := spill.read(b, func(index int, program guide.Program) {
			if state := states[index]; !state.failed {
				state.programs = append(state.programs, program)
			}
		})
		if err != nil {
			return result, err
		}
		first, last := spill.bounds(b, len(states))
		for _, state := range states[first:last] {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if state.failed {
				continue
			}
			var previous map[dto.UUID]item.Item
			if state.existed {
				children, err := s.options.Items.Children(ctx, state.item.ID)
				if err != nil {
					return result, fmt.Errorf("livetv: read programmes: %w", err)
				}
				previous = byID(children)
			}
			programs := programItems(state, previous, now)
			state.programs = nil
			result.Programs += len(programs)
			changes, err := s.options.Items.ReplaceChildren(ctx, state.item.ID, programs)
			if err != nil {
				return result, fmt.Errorf("livetv: write programmes of %s: %w", state.tuner.ID, err)
			}
			result.Changes.add(changes)
			report(15 + 85*float64(state.index+1)/float64(len(states)))
		}
		spill.release(b)
	}
	report(100)
	s.options.Logger.InfoContext(ctx, "guide refreshed", "channels", result.Channels, "programs", result.Programs,
		"inserted", result.Inserted, "updated", result.Updated, "deleted", result.Deleted, "kept", result.Kept)
	return result, nil
}

// tunerChannels lists the channels of every configured tuner, in
// configuration order, without duplicate ids (DefaultLiveTvService and
// BaseTunerHost.GetChannels). kept is true when a tuner failed with no
// earlier list.
func (s *Service) tunerChannels(ctx context.Context, options config.LiveTvOptions) (channels []tuner.Channel, kept bool) {
	seen := make(map[string]struct{})
	for _, info := range options.TunerHosts {
		host, ok := s.options.Tuners.Host(info.Type)
		if !ok {
			continue
		}
		list, err := host.Channels(ctx, info)
		if err != nil {
			s.options.Logger.ErrorContext(ctx, "error getting channel list", "tuner", info.Id, "error", err)
			if list == nil {
				kept = true
				continue
			}
		}
		for _, channel := range list {
			key := strings.ToUpper(channel.ID)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			channels = append(channels, channel)
		}
	}
	return channels, kept
}

// epgChannels is the guide channel list of one provider, or the error of
// reading it.
type epgChannels struct {
	channels *guide.Channels
	err      error
}

// addProviderMetadata reads each provider's guide channels and puts a guide
// logo on the tuner channels it matches (ListingsManager.AddProviderMetadata
// and AddMetadata, ListingsManager.cs:177-308).
func (s *Service) addProviderMetadata(ctx context.Context, providers []configuredListings, channels []tuner.Channel) map[string]epgChannels {
	out := make(map[string]epgChannels, len(providers))
	for _, configured := range providers {
		enabled := false
		for _, channel := range channels {
			if guide.EnabledForTuner(configured.info, channel.TunerHostID) {
				enabled = true
				break
			}
		}
		if !enabled {
			continue
		}
		list, err := configured.provider.Channels(ctx, configured.info, s.language(configured.info))
		if err != nil {
			s.options.Logger.ErrorContext(ctx, "error adding guide metadata", "provider", configured.info.Id, "error", err)
			out[configured.info.Id] = epgChannels{err: err}
			continue
		}
		index := guide.NewChannels(list)
		out[configured.info.Id] = epgChannels{channels: index}
		for i := range channels {
			if !guide.EnabledForTuner(configured.info, channels[i].TunerHostID) {
				continue
			}
			match, ok := index.Match(configured.info.ChannelMappings, channels[i])
			if ok && strings.TrimSpace(match.ImageURL) != "" {
				channels[i].ImageURL = match.ImageURL
			}
		}
	}
	return out
}

// readPrograms reads one provider's programmes for every channel that has
// none yet, in one pass over the guide. It ports the per-channel loop of
// ListingsManager.GetProgramsAsync (ListingsManager.cs:119-175): the guide
// id gets "_" and the tuner channel id appended, and ChannelId becomes the
// tuner channel id.
func (s *Service) readPrograms(ctx context.Context, configured configuredListings, epg epgChannels, states []*channelState, start, end time.Time, spill *programSpill) error {
	var candidates []*channelState
	routes := make(map[string][]*channelState)
	var ids []string
	for _, state := range states {
		if state.done || state.failed || !guide.EnabledForTuner(configured.info, state.tuner.TunerHostID) {
			continue
		}
		if epg.err != nil || epg.channels == nil {
			// GetEpgChannels throws again for every channel.
			state.failed = epg.err != nil
			continue
		}
		match, ok := epg.channels.Match(configured.info.ChannelMappings, state.tuner)
		if !ok {
			continue
		}
		key := strings.ToUpper(match.ID)
		if _, seen := routes[key]; !seen {
			ids = append(ids, match.ID)
		}
		routes[key] = append(routes[key], state)
		candidates = append(candidates, state)
	}
	if len(candidates) == 0 {
		return nil
	}
	var spillErr error
	err := configured.provider.Programs(ctx, configured.info, s.language(configured.info), ids, start, end, func(program guide.Program) error {
		for _, state := range routes[strings.ToUpper(program.ChannelID)] {
			routed := program
			routed.ChannelID = state.tuner.ID
			routed.ID = program.ID + "_" + state.tuner.ID
			if spillErr = spill.add(state.index, routed); spillErr != nil {
				return spillErr
			}
			state.count++
		}
		return nil
	})
	if spillErr != nil {
		return spillErr
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.options.Logger.ErrorContext(ctx, "error getting programs", "provider", configured.info.Id, "error", err)
		for _, state := range candidates {
			state.count = 0
			state.failed = true
		}
		return nil
	}
	for _, state := range candidates {
		state.done = state.count > 0
	}
	return nil
}

// programItems maps a channel's programmes to items. A repeated programme
// id keeps its first position and its last data, as upstream writes the
// later copy over the earlier one.
func programItems(state *channelState, previous map[dto.UUID]item.Item, now time.Time) []item.Item {
	out := make([]item.Item, 0, len(state.programs))
	position := make(map[dto.UUID]int, len(state.programs))
	for _, program := range state.programs {
		var stored *item.Item
		if value, ok := previous[catalog.ProgramID(program.ID)]; ok {
			stored = &value
		}
		value := catalog.Program(program, state.item, stored, now)
		if index, dup := position[value.ID]; dup {
			out[index] = value
			continue
		}
		position[value.ID] = len(out)
		out = append(out, value)
	}
	return out
}

// channelInfo maps a tuner channel to the catalog input. An M3U
// group-title becomes the channel's tags (D-90); upstream parses it and
// drops it (M3uParser.cs:137-139). The tag has each "|" replaced with "/",
// because the "tags" query parameter splits on "|"; group filters read the
// raw group-title from the tuner channel.
func channelInfo(channel tuner.Channel) catalog.ChannelInfo {
	info := catalog.ChannelInfo{
		ID:       channel.ID,
		Name:     channel.Name,
		Number:   channel.Number,
		ImageURL: channel.ImageURL,
	}
	if group := strings.TrimSpace(channel.Group); group != "" {
		info.Tags = []string{strings.ReplaceAll(group, "|", "/")}
	}
	if channel.Type == tuner.ChannelTypeRadio {
		info.ChannelType = item.ChannelTypeRadio
	}
	return info
}

func byID(values []item.Item) map[dto.UUID]item.Item {
	out := make(map[dto.UUID]item.Item, len(values))
	for _, value := range values {
		out[value.ID] = value
	}
	return out
}
