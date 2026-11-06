// SPDX-License-Identifier: GPL-2.0-only

package livetv_test

import (
	"errors"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/livetv"
)

// A stored channel resolves to its tuner and current playlist entry; other
// items are not channels, and a channel whose tuner is gone is not found.
func TestChannelStream(t *testing.T) {
	f := newFixture(t, fakehdhrOptions(t), nil)
	if _, err := f.service.RefreshGuide(t.Context(), func(float64) {}); err != nil {
		t.Fatal(err)
	}
	var channel item.Item
	for _, value := range f.items.items {
		if value.Kind == item.KindLiveTvChannel {
			channel = value
			break
		}
	}
	if channel.ExternalID == nil {
		t.Fatal("no stored channel")
	}

	stream, ok, err := f.service.ChannelStream(t.Context(), channel.ID)
	if err != nil || !ok {
		t.Fatalf("channel stream: %v %v", ok, err)
	}
	if stream.Tuner.Id != "tuner1" || stream.Channel.ID != *channel.ExternalID || stream.Channel.Path == "" {
		t.Fatalf("stream %+v", stream)
	}

	folder := f.service.FolderID()
	if _, ok, err := f.service.ChannelStream(t.Context(), folder); ok || err != nil {
		t.Fatalf("folder is a channel: %v %v", ok, err)
	}

	if err := f.config.UpdateLiveTv(t.Context(), func(options *config.LiveTvOptions) error {
		options.TunerHosts = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.service.ChannelStream(t.Context(), channel.ID); !ok || !errors.Is(err, livetv.ErrNotFound) {
		t.Fatalf("channel without tuner: %v %v", ok, err)
	}
}
