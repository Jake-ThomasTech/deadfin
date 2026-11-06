// SPDX-License-Identifier: GPL-2.0-only

package guide

import (
	"strings"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// Channel is a guide (EPG) channel: the fields of the ChannelInfo that
// XmlTvListingsProvider.GetChannels fills (XmlTvListingsProvider.cs:305-321).
type Channel struct {
	ID       string
	Name     string
	Number   string
	ImageURL string
}

// Channels indexes the guide channels of one listing provider by id, number
// and normalized name, as EpgChannelData does (EpgChannelData.cs). Lookups
// are case-insensitive; a later duplicate wins.
type Channels struct {
	byID     map[string]Channel
	byNumber map[string]Channel
	byName   map[string]Channel
	all      []Channel
}

// NewChannels indexes channels.
func NewChannels(channels []Channel) *Channels {
	index := &Channels{
		byID:     make(map[string]Channel, len(channels)),
		byNumber: make(map[string]Channel, len(channels)),
		byName:   make(map[string]Channel, len(channels)),
		all:      channels,
	}
	for _, channel := range channels {
		index.byID[foldKey(channel.ID)] = channel
		if channel.Number != "" {
			index.byNumber[foldKey(channel.Number)] = channel
		}
		if name := NormalizeName(channel.Name); strings.TrimSpace(name) != "" {
			index.byName[foldKey(name)] = channel
		}
	}
	return index
}

// All returns the channels in file order.
func (c *Channels) All() []Channel { return c.all }

// NormalizeName removes spaces and hyphens (EpgChannelData.cs:49-52).
func NormalizeName(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, " ", ""), "-", "")
}

// Match finds the guide channel of a tuner channel. It ports
// ListingsManager.GetEpgChannelFromTunerChannel (ListingsManager.cs:387-455):
// the tuner channel id, then the tuner channel id from the playlist (tvg-id),
// then the number, each through the provider's ChannelMappings, and last the
// normalized name.
func (c *Channels) Match(mappings []config.NameValuePair, channel tuner.Channel) (Channel, bool) {
	if strings.TrimSpace(channel.ID) != "" {
		if found, ok := c.byID[foldKey(mappedChannel(channel.ID, mappings))]; ok {
			return found, true
		}
	}
	if strings.TrimSpace(channel.TunerChannelID) != "" {
		key := channel.TunerChannelID
		if containsFold(key, ".json.schedulesdirect.org") {
			key = strings.TrimLeft(replaceFold(key, ".json.schedulesdirect.org"), "I")
		}
		if found, ok := c.byID[foldKey(mappedChannel(key, mappings))]; ok {
			return found, true
		}
	}
	if strings.TrimSpace(channel.Number) != "" {
		if found, ok := c.byNumber[foldKey(mappedChannel(channel.Number, mappings))]; ok {
			return found, true
		}
	}
	if strings.TrimSpace(channel.Name) != "" {
		if found, ok := c.byName[foldKey(NormalizeName(channel.Name))]; ok {
			return found, true
		}
	}
	return Channel{}, false
}

// mappedChannel ports ListingsManager.GetMappedChannel
// (ListingsManager.cs:322-333) with the blank-value fallback of its callers.
func mappedChannel(id string, mappings []config.NameValuePair) string {
	for _, mapping := range mappings {
		if strings.EqualFold(mapping.Name, id) {
			if strings.TrimSpace(mapping.Value) == "" {
				return id
			}
			return mapping.Value
		}
	}
	return id
}

// EnabledForTuner ports ListingsManager.IsListingProviderEnabledForTuner
// (ListingsManager.cs:310-320).
func EnabledForTuner(provider config.ListingsProviderInfo, tunerHostID string) bool {
	if provider.EnableAllTuners {
		return true
	}
	for _, id := range provider.EnabledTuners {
		if strings.EqualFold(id, tunerHostID) {
			return true
		}
	}
	return false
}

// foldKey is the map key for an OrdinalIgnoreCase dictionary.
func foldKey(value string) string { return strings.ToUpper(value) }

func containsFold(value, pattern string) bool {
	return strings.Contains(strings.ToUpper(value), strings.ToUpper(pattern))
}

// replaceFold removes every case-insensitive occurrence of pattern.
func replaceFold(value, pattern string) string {
	upperPattern := strings.ToUpper(pattern)
	var out strings.Builder
	for {
		index := strings.Index(strings.ToUpper(value), upperPattern)
		if index < 0 || len(strings.ToUpper(value)) != len(value) {
			out.WriteString(value)
			return out.String()
		}
		out.WriteString(value[:index])
		value = value[index+len(pattern):]
	}
}
