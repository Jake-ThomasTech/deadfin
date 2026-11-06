// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/guide"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// NameID is MediaBrowser.Model.Dto.NameIdPair.
type NameID struct {
	Name string
	ID   string
}

// TunerChannelMapping is MediaBrowser.Model.LiveTv.TunerChannelMapping.
type TunerChannelMapping struct {
	Name                string
	ProviderChannelName string
	ProviderChannelID   string
	ID                  string
}

// ChannelMappingOptions is Jellyfin.Api.Models.LiveTvDtos.ChannelMappingOptionsDto.
type ChannelMappingOptions struct {
	TunerChannels    []TunerChannelMapping
	ProviderChannels []NameID
	Mappings         []config.NameValuePair
	ProviderName     string
}

// GuideInfo is the guide date range (GuideManager.GetGuideInfo).
func (s *Service) GuideInfo() (start, end time.Time) {
	start = s.options.Clock.Now().UTC()
	return start, start.AddDate(0, 0, guide.Days(s.options.Config.LiveTv().GuideDays))
}

// TunerHostTypes lists the tuner types (TunerHostManager.GetTunerHostTypes).
func (s *Service) TunerHostTypes() []tuner.TypeInfo {
	return s.options.Tuners.Types()
}

// SaveTunerHost adds or replaces a tuner and queues a guide refresh. It ports
// TunerHostManager.SaveTunerHost (TunerHostManager.cs:61-100). A blank or
// unknown Id gets a new id. The id is set before validation, so the
// validating download and the next refresh share one cached channel list.
// A save of a stored tuner keeps the Deadfin-only fields the request omits
// (D-78) and clears the tuner's cache and fetch state first, so a save is
// also the retry of a rejected source.
func (s *Service) SaveTunerHost(ctx context.Context, info config.TunerHostInfo) (config.TunerHostInfo, error) {
	host, ok := s.options.Tuners.Host(info.Type)
	if !ok {
		return config.TunerHostInfo{}, fmt.Errorf("%w: tuner type %q", ErrUnknownType, info.Type)
	}
	current := s.options.Config.LiveTv()
	index := slices.IndexFunc(current.TunerHosts, func(i config.TunerHostInfo) bool { return equalFold(i.Id, info.Id) })
	if index == -1 || strings.TrimSpace(info.Id) == "" {
		info.Id = s.options.NewID()
	} else {
		previous := current.TunerHosts[index]
		info.KeepDeadfinFields(previous)
		if !groupsOnlyChange(previous, info) {
			s.forget(previous)
		}
	}
	if v, ok := host.(validator); ok {
		if err := v.Validate(ctx, info); err != nil {
			return config.TunerHostInfo{}, err
		}
	}
	err := s.options.Config.UpdateLiveTv(ctx, func(options *config.LiveTvOptions) error {
		index := slices.IndexFunc(options.TunerHosts, func(i config.TunerHostInfo) bool { return equalFold(i.Id, info.Id) })
		if index == -1 {
			options.TunerHosts = append(options.TunerHosts, info)
			return nil
		}
		options.TunerHosts[index] = info
		return nil
	})
	if err != nil {
		return config.TunerHostInfo{}, err
	}
	s.queue()
	return info, nil
}

// groupsOnlyChange reports a save that changes only the group filter. Such
// a save keeps the cached playlist, so it downloads nothing (D-91).
func groupsOnlyChange(previous, next config.TunerHostInfo) bool {
	if slices.Equal(previous.IncludeGroups, next.IncludeGroups) && slices.Equal(previous.ExcludeGroups, next.ExcludeGroups) {
		return false
	}
	previous.IncludeGroups, previous.ExcludeGroups = nil, nil
	next.IncludeGroups, next.ExcludeGroups = nil, nil
	return reflect.DeepEqual(previous, next)
}

// TunerHostGroups are the group-titles of a tuner's cached playlist.
type TunerHostGroups struct {
	Groups []tuner.GroupCount
	// Loaded is false when no playlist is cached yet; a guide refresh or a
	// tuner save loads it.
	Loaded bool
}

// grouper is a tuner type that counts its channels by group.
type grouper interface {
	Groups(tuner config.TunerHostInfo) ([]tuner.GroupCount, bool)
}

// TunerHostGroups counts the channels of a tuner's cached playlist per
// group-title, before the group filter (D-91). It never contacts the
// source. A tuner type without groups returns none.
func (s *Service) TunerHostGroups(id string) (TunerHostGroups, error) {
	tuners := s.options.Config.LiveTv().TunerHosts
	index := slices.IndexFunc(tuners, func(i config.TunerHostInfo) bool { return equalFold(i.Id, id) })
	if strings.TrimSpace(id) == "" || index == -1 {
		return TunerHostGroups{}, fmt.Errorf("%w: tuner %q", ErrNotFound, id)
	}
	info := tuners[index]
	host, ok := s.options.Tuners.Host(info.Type)
	if !ok {
		return TunerHostGroups{}, fmt.Errorf("%w: tuner type %q", ErrUnknownType, info.Type)
	}
	g, ok := host.(grouper)
	if !ok {
		return TunerHostGroups{Groups: []tuner.GroupCount{}, Loaded: true}, nil
	}
	groups, loaded := g.Groups(info)
	return TunerHostGroups{Groups: groups, Loaded: loaded}, nil
}

// DeleteTunerHost removes a tuner and queues a guide refresh
// (TunerHostManager.DeleteTunerHost, TunerHostManager.cs:103-128). An
// unknown id changes nothing but still queues the refresh.
func (s *Service) DeleteTunerHost(ctx context.Context, id string) error {
	var removed []config.TunerHostInfo
	err := s.options.Config.UpdateLiveTv(ctx, func(options *config.LiveTvOptions) error {
		kept := options.TunerHosts[:0:0]
		for _, info := range options.TunerHosts {
			if equalFold(info.Id, id) {
				removed = append(removed, info)
				continue
			}
			kept = append(kept, info)
		}
		options.TunerHosts = kept
		return nil
	})
	if err != nil {
		return err
	}
	for _, info := range removed {
		s.forget(info)
	}
	s.queue()
	return nil
}

func (s *Service) forget(info config.TunerHostInfo) {
	if host, ok := s.options.Tuners.Host(info.Type); ok {
		if f, ok := host.(forgetter); ok {
			f.Forget(info)
		}
	}
}

// SaveListingProvider adds or replaces a listing provider, drops its cached
// guide and queues a guide refresh (ListingsManager.SaveListingProvider,
// ListingsManager.cs:55-83).
func (s *Service) SaveListingProvider(ctx context.Context, info config.ListingsProviderInfo) (config.ListingsProviderInfo, error) {
	provider, ok := s.listingsProvider(info.Type)
	if !ok {
		return config.ListingsProviderInfo{}, fmt.Errorf("%w: listing provider type %q", ErrUnknownType, info.Type)
	}
	if err := provider.Validate(info); err != nil {
		return config.ListingsProviderInfo{}, err
	}
	var previous *config.ListingsProviderInfo
	err := s.options.Config.UpdateLiveTv(ctx, func(options *config.LiveTvOptions) error {
		index := slices.IndexFunc(options.ListingProviders, func(i config.ListingsProviderInfo) bool { return equalFold(i.Id, info.Id) })
		if index == -1 || strings.TrimSpace(info.Id) == "" {
			info.Id = s.options.NewID()
			options.ListingProviders = append(options.ListingProviders, info)
			return nil
		}
		old := options.ListingProviders[index]
		previous = &old
		info.KeepDeadfinFields(old)
		options.ListingProviders[index] = info
		return nil
	})
	if err != nil {
		return config.ListingsProviderInfo{}, err
	}
	if previous != nil {
		s.forgetListings(*previous)
	}
	s.forgetListings(info)
	s.invalidateListings(ctx, info.Id)
	s.queue()
	return info, nil
}

// DeleteListingProvider removes a listing provider, drops its cached guide
// and queues a guide refresh (ListingsManager.DeleteListingsProvider,
// ListingsManager.cs:86-101).
func (s *Service) DeleteListingProvider(ctx context.Context, id string) error {
	var removed []config.ListingsProviderInfo
	err := s.options.Config.UpdateLiveTv(ctx, func(options *config.LiveTvOptions) error {
		options.ListingProviders = slices.DeleteFunc(options.ListingProviders, func(i config.ListingsProviderInfo) bool {
			if equalFold(i.Id, id) {
				removed = append(removed, i)
				return true
			}
			return false
		})
		return nil
	})
	if err != nil {
		return err
	}
	for _, info := range removed {
		s.forgetListings(info)
	}
	if id != "" {
		s.invalidateListings(ctx, id)
	}
	s.queue()
	return nil
}

// forgetListings clears the fetch state of a listing provider, when the
// provider type keeps one.
func (s *Service) forgetListings(info config.ListingsProviderInfo) {
	if provider, ok := s.listingsProvider(info.Type); ok {
		if f, ok := provider.(interface {
			Forget(config.ListingsProviderInfo)
		}); ok {
			f.Forget(info)
		}
	}
}

// invalidateListings deletes the cached XMLTV copy of a provider
// (ListingsManager.InvalidateListingsProviderCache,
// ListingsManager.cs:335-362). Only a Guid "N" id names a file, so request
// input never reaches a path.
func (s *Service) invalidateListings(ctx context.Context, id string) {
	safe, ok := guidN(id)
	if !ok || s.options.Paths.Cache == "" {
		return
	}
	err := os.Remove(filepath.Join(s.options.Paths.Cache, "xmltv", safe+".xml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.options.Logger.WarnContext(ctx, "error deleting XMLTV cache file", "provider", safe, "error", err)
	}
}

// guidN parses a Guid "N" string (32 hex digits) and returns it in lower
// case, as Guid.TryParseExact(id, "N") and ToString("N") do.
func guidN(id string) (string, bool) {
	if len(id) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	return strings.ToLower(id), true
}

// Lineups lists the lineups of a provider (ListingsManager.GetLineups,
// ListingsManager.cs:104-116). An XMLTV file has one lineup per guide
// channel (XmlTvListingsProvider.GetLineups). Without providerID upstream
// calls the provider with no configuration, which XMLTV cannot serve.
func (s *Service) Lineups(ctx context.Context, providerType, providerID string) ([]NameID, error) {
	if strings.TrimSpace(providerID) == "" {
		if _, ok := s.listingsProvider(providerType); !ok {
			return nil, fmt.Errorf("%w: listing provider type %q", ErrUnknownType, providerType)
		}
		return nil, fmt.Errorf("%w: listing provider id is required", ErrNotFound)
	}
	info, ok := s.listingsInfo(providerID)
	if !ok {
		return nil, fmt.Errorf("%w: listing provider %q", ErrNotFound, providerID)
	}
	provider, ok := s.listingsProvider(info.Type)
	if !ok {
		return nil, fmt.Errorf("%w: listing provider type %q", ErrUnknownType, info.Type)
	}
	channels, err := provider.Channels(ctx, info, s.language(info))
	if err != nil {
		return nil, err
	}
	out := make([]NameID, 0, len(channels))
	for _, channel := range channels {
		out = append(out, NameID{Name: channel.Name, ID: channel.ID})
	}
	return out, nil
}

// ChannelMappingOptions returns the tuner channels of a provider with their
// guide matches (ListingsManager.GetChannelMappingOptions,
// ListingsManager.cs:204-230).
func (s *Service) ChannelMappingOptions(ctx context.Context, providerID string) (ChannelMappingOptions, error) {
	info, ok := s.listingsInfo(providerID)
	if !ok {
		return ChannelMappingOptions{}, fmt.Errorf("%w: listing provider %q", ErrNotFound, providerID)
	}
	provider, ok := s.listingsProvider(info.Type)
	if !ok {
		return ChannelMappingOptions{}, fmt.Errorf("%w: listing provider type %q", ErrUnknownType, info.Type)
	}
	tunerChannels := s.channelsForListings(ctx, info)
	providerChannels, err := provider.Channels(ctx, info, s.language(info))
	if err != nil {
		return ChannelMappingOptions{}, err
	}
	index := guide.NewChannels(providerChannels)
	result := ChannelMappingOptions{
		TunerChannels:    make([]TunerChannelMapping, 0, len(tunerChannels)),
		ProviderChannels: make([]NameID, 0, len(providerChannels)),
		Mappings:         nonNilMappings(info.ChannelMappings),
		ProviderName:     provider.Name(),
	}
	for _, channel := range tunerChannels {
		result.TunerChannels = append(result.TunerChannels, tunerChannelMapping(channel, info.ChannelMappings, index))
	}
	for _, channel := range providerChannels {
		result.ProviderChannels = append(result.ProviderChannels, NameID{Name: channel.Name, ID: channel.ID})
	}
	return result, nil
}

// SetChannelMapping toggles the mapping of a tuner channel to a guide
// channel and queues a guide refresh (ListingsManager.SetChannelMapping,
// ListingsManager.cs:233-275). Setting an existing pair, or a channel to
// itself, removes the mapping.
func (s *Service) SetChannelMapping(ctx context.Context, providerID, tunerChannelID, providerChannelID string) (TunerChannelMapping, error) {
	var saved config.ListingsProviderInfo
	err := s.options.Config.UpdateLiveTv(ctx, func(options *config.LiveTvOptions) error {
		index := slices.IndexFunc(options.ListingProviders, func(i config.ListingsProviderInfo) bool { return equalFold(i.Id, providerID) })
		if index == -1 {
			return fmt.Errorf("%w: listing provider %q", ErrNotFound, providerID)
		}
		info := &options.ListingProviders[index]
		exists := slices.ContainsFunc(info.ChannelMappings, func(pair config.NameValuePair) bool {
			return equalFold(pair.Name, tunerChannelID) && equalFold(pair.Value, providerChannelID)
		})
		mappings := slices.DeleteFunc(slices.Clone(info.ChannelMappings), func(pair config.NameValuePair) bool {
			return equalFold(pair.Name, tunerChannelID)
		})
		if !equalFold(tunerChannelID, providerChannelID) && !exists {
			mappings = append(mappings, config.NameValuePair{Name: tunerChannelID, Value: providerChannelID})
		}
		info.ChannelMappings = nonNilMappings(mappings)
		saved = *info
		return nil
	})
	if err != nil {
		return TunerChannelMapping{}, err
	}
	provider, ok := s.listingsProvider(saved.Type)
	if !ok {
		return TunerChannelMapping{}, fmt.Errorf("%w: listing provider type %q", ErrUnknownType, saved.Type)
	}
	tunerChannels := s.channelsForListings(ctx, saved)
	providerChannels, err := provider.Channels(ctx, saved, s.language(saved))
	if err != nil {
		return TunerChannelMapping{}, err
	}
	s.queue()
	index := guide.NewChannels(providerChannels)
	for _, channel := range tunerChannels {
		if equalFold(channel.ID, tunerChannelID) {
			return tunerChannelMapping(channel, saved.ChannelMappings, index), nil
		}
	}
	return TunerChannelMapping{}, fmt.Errorf("%w: tuner channel %q", ErrNotFound, tunerChannelID)
}

func (s *Service) listingsInfo(id string) (config.ListingsProviderInfo, bool) {
	for _, info := range s.options.Config.LiveTv().ListingProviders {
		if equalFold(info.Id, id) {
			return info, true
		}
	}
	return config.ListingsProviderInfo{}, false
}

// channelsForListings lists the tuner channels a provider serves
// (ListingsManager.GetChannelsForListingsProvider,
// ListingsManager.cs:480-498). A failed tuner is skipped.
func (s *Service) channelsForListings(ctx context.Context, info config.ListingsProviderInfo) []tuner.Channel {
	var out []tuner.Channel
	for _, host := range s.options.Config.LiveTv().TunerHosts {
		implementation, ok := s.options.Tuners.Host(host.Type)
		if !ok {
			continue
		}
		channels, err := implementation.Channels(ctx, host)
		if err != nil {
			s.options.Logger.ErrorContext(ctx, "error getting channels", "tuner", host.Id, "error", err)
			continue
		}
		for _, channel := range channels {
			if guide.EnabledForTuner(info, channel.TunerHostID) {
				out = append(out, channel)
			}
		}
	}
	return out
}

// tunerChannelMapping ports ListingsManager.GetTunerChannelMapping
// (ListingsManager.cs:457-478).
func tunerChannelMapping(channel tuner.Channel, mappings []config.NameValuePair, index *guide.Channels) TunerChannelMapping {
	result := TunerChannelMapping{Name: channel.Name, ID: channel.ID}
	if strings.TrimSpace(channel.Number) != "" {
		result.Name = channel.Number + " " + result.Name
	}
	if match, ok := index.Match(mappings, channel); ok {
		result.ProviderChannelName = match.Name
		result.ProviderChannelID = match.ID
	}
	return result
}

func nonNilMappings(mappings []config.NameValuePair) []config.NameValuePair {
	if mappings == nil {
		return []config.NameValuePair{}
	}
	return mappings
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }

// Hidden reports that no tuner is configured, so the Refresh Guide task is
// hidden (RefreshGuideScheduledTask.IsHidden).
func (s *Service) Hidden() bool {
	return len(s.options.Config.LiveTv().TunerHosts) == 0
}
