// SPDX-License-Identifier: GPL-2.0-only

// Package tuner lists the channels of configured Live TV tuners. A Host
// implements one tuner type ("m3u" now; the HDHomeRun and Xtream types plug
// into the same seam later, docs/spikes/livetv.md §4.3). Hosts are
// context-first, take an injected HTTP path and hold no package state; a
// Registry composed in cmd/deadfin holds the available types.
package tuner

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/deadfin-org/deadfin/internal/config"
)

// ChannelType is MediaBrowser.Model.LiveTv.ChannelType.
type ChannelType string

const (
	ChannelTypeTV    ChannelType = "TV"
	ChannelTypeRadio ChannelType = "Radio"
)

// Channel is one tuner channel. It carries the fields of
// MediaBrowser.Controller/LiveTv/ChannelInfo.cs that the tuner hosts fill.
type Channel struct {
	// ID is the upstream channel id, for example m3u_<md5 url><md5 path>.
	ID string
	// Name is the display name.
	Name string
	// Number is the channel number text, for example "4.1"; empty if none.
	Number string
	// Path is the stream URL. It may carry provider credentials.
	Path string
	// ImageURL is the channel logo URL.
	ImageURL string
	// Group is the M3U group-title.
	Group string
	// TunerChannelID is the guide match key (M3U tvg-id, else channel-id).
	TunerChannelID string
	// TunerHostID is the configured tuner id.
	TunerHostID string
	// Type is TV or Radio.
	Type ChannelType
}

// Host is one tuner type. It ports the channel half of ITunerHost
// (MediaBrowser.Controller/LiveTv/ITunerHost.cs); opening streams follows
// in phase 2.
type Host interface {
	// Type is the TunerHostInfo.Type value, for example "m3u".
	Type() string
	// Name is the display name listed by /LiveTv/TunerHosts/Types.
	Name() string
	// Channels lists the channels of one configured tuner.
	Channels(ctx context.Context, tuner config.TunerHostInfo) ([]Channel, error)
}

// TypeInfo is one entry of GET /LiveTv/TunerHosts/Types (NameIdPair).
type TypeInfo struct {
	Name string
	ID   string
}

// Registry holds the available tuner types in registration order.
type Registry struct {
	hosts []Host
}

// NewRegistry returns a registry of hosts.
func NewRegistry(hosts ...Host) *Registry {
	return &Registry{hosts: slices.Clone(hosts)}
}

// Host returns the host of a tuner type. The match is case-insensitive, as
// TunerHostManager matches Type (TunerHostManager.cs).
func (r *Registry) Host(tunerType string) (Host, bool) {
	for _, host := range r.hosts {
		if strings.EqualFold(host.Type(), tunerType) {
			return host, true
		}
	}
	return nil, false
}

// Types lists the tuner types ordered by name
// (TunerHostManager.GetTunerHostTypes, TunerHostManager.cs:54-59).
func (r *Registry) Types() []TypeInfo {
	types := make([]TypeInfo, 0, len(r.hosts))
	for _, host := range r.hosts {
		types = append(types, TypeInfo{Name: host.Name(), ID: host.Type()})
	}
	slices.SortStableFunc(types, func(a, b TypeInfo) int { return strings.Compare(a.Name, b.Name) })
	return types
}

// Streamer is a tuner type that opens channel streams. It ports the stream
// half of ITunerHost (GetChannelStream).
type Streamer interface {
	// Owns reports whether channelID belongs to the tuner, from the id
	// prefix alone (BaseTunerHost.IsValidChannelId), so a lookup does not
	// download the playlists of other tuners.
	Owns(tuner config.TunerHostInfo, channelID string) bool
	// Stream opens the byte stream of a channel of the tuner.
	Stream(ctx context.Context, tuner config.TunerHostInfo, channel Channel) (io.ReadCloser, error)
}
