// SPDX-License-Identifier: GPL-2.0-only

package dtobuild_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/dtobuild"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/store"
)

// TestBuildProjectsLiveTvItems pins the AddInfoToProgramDto and
// AddChannelInfo facts that need no other item.
func TestBuildProjectsLiveTvItems(t *testing.T) {
	number, title := "4.1", "Pilot"
	start := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	channel := item.Item{
		ID: mustUUID(t, "10000000000000000000000000000001"), Kind: item.KindLiveTvChannel, Name: ptr("Radio One"),
		Extra: item.ExtraFields{LiveTV: &item.LiveTVExtra{ChannelType: item.ChannelTypeRadio, Number: &number}},
	}
	program := item.Item{
		ID: mustUUID(t, "10000000000000000000000000000002"), Kind: item.KindLiveTvProgram, Name: ptr("Show"),
		StartDate: &start, EpisodeTitle: &title, IsSeries: true, ChannelID: &channel.ID,
		Values: item.ItemValues{Tags: []string{"sports", "Live"}},
	}
	page, err := dtobuild.NewBuilder(nil, nil).Build(t.Context(), []item.Item{channel, program}, store.ItemQueryDTOOptions{AddCurrentProgram: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gotChannel, gotProgram := page.Items[0], page.Items[1]
	if gotChannel.Type != "TvChannel" || gotChannel.Number != number || gotChannel.ChannelNumber != number || gotChannel.ChannelType == nil || *gotChannel.ChannelType != "Radio" || gotChannel.MediaType != "Audio" {
		t.Fatalf("channel %+v", gotChannel)
	}
	encoded, err := json.Marshal(gotProgram)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"Type": "Program", "EpisodeTitle": title, "IsSeries": true, "IsSports": true, "IsLive": true} {
		if fields[key] != want {
			t.Errorf("program %s = %v, want %v", key, fields[key], want)
		}
	}
	// A false flag stays null: the source ORs a bool into a bool?.
	for _, key := range []string{"IsMovie", "IsNews", "IsKids", "IsPremiere", "IsRepeat", "LocationType"} {
		if _, ok := fields[key]; ok {
			t.Errorf("program %s = %v, want absent", key, fields[key])
		}
	}
	if gotProgram.StartDate == nil || !gotProgram.StartDate.Equal(start) {
		t.Errorf("program StartDate %v", gotProgram.StartDate)
	}
}

func mustUUID(t *testing.T, value string) dto.UUID {
	t.Helper()
	id, err := dto.ParseUUID(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ptr[T any](value T) *T { return &value }
