// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// ChannelStream is the tuner side of a stored channel: the configured
// tuner and the tuner's current entry for the channel. Channel.Path may
// carry provider credentials; it must not leave the server.
type ChannelStream struct {
	Item    item.Item
	Tuner   config.TunerHostInfo
	Channel tuner.Channel
}

// ChannelStream resolves a stored Live TV channel to its tuner channel
// (LiveTvMediaSourceProvider.GetChannelStream and
// BaseTunerHost.GetChannelStream). ok is false when the item is not a Live
// TV channel. A channel whose tuner or playlist entry is gone is
// ErrNotFound. The tuner's playlist comes from its cache when it is fresh.
func (s *Service) ChannelStream(ctx context.Context, itemID dto.UUID) (stream ChannelStream, ok bool, err error) {
	value, exists, err := s.options.Items.Item(ctx, itemID)
	if err != nil {
		return ChannelStream{}, false, err
	}
	if !exists || value.Kind != item.KindLiveTvChannel {
		return ChannelStream{}, false, nil
	}
	if value.ExternalID == nil || *value.ExternalID == "" {
		return ChannelStream{}, true, fmt.Errorf("%w: channel has no tuner id", ErrNotFound)
	}
	externalID := *value.ExternalID
	var lookupErr error
	for _, info := range s.options.Config.LiveTv().TunerHosts {
		host, found := s.options.Tuners.Host(info.Type)
		if !found {
			continue
		}
		streamer, isStreamer := host.(tuner.Streamer)
		if !isStreamer || !streamer.Owns(info, externalID) {
			continue
		}
		// A failed download returns the last good list with the error.
		channels, err := host.Channels(ctx, info)
		if err != nil && len(channels) == 0 {
			lookupErr = errors.Join(lookupErr, err)
			continue
		}
		for _, channel := range channels {
			if channel.ID == externalID {
				return ChannelStream{Item: value, Tuner: info, Channel: channel}, true, nil
			}
		}
	}
	if lookupErr != nil {
		return ChannelStream{}, true, lookupErr
	}
	return ChannelStream{}, true, fmt.Errorf("%w: channel is not on a configured tuner", ErrNotFound)
}

// OpenStream opens the byte stream of a channel through its tuner type.
func (s *Service) OpenStream(ctx context.Context, stream ChannelStream) (io.ReadCloser, error) {
	host, found := s.options.Tuners.Host(stream.Tuner.Type)
	if !found {
		return nil, fmt.Errorf("%w: tuner type %q", ErrUnknownType, stream.Tuner.Type)
	}
	streamer, isStreamer := host.(tuner.Streamer)
	if !isStreamer {
		return nil, fmt.Errorf("%w: tuner type %q opens no streams", ErrUnknownType, stream.Tuner.Type)
	}
	return streamer.Stream(ctx, stream.Tuner, stream.Channel)
}
