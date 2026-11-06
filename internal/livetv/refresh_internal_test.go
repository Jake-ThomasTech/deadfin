// SPDX-License-Identifier: GPL-2.0-only

package livetv

import (
	"slices"
	"testing"

	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

// D-90: a group-title becomes the channel tag with each "|" replaced, so
// the pipe-delimited "tags" query parameter can match it.
func TestChannelInfoTagReplacesPipes(t *testing.T) {
	for group, want := range map[string][]string{
		"US | Entertainment": {"US / Entertainment"},
		"A|B":                {"A/B"},
		" News ":             {"News"},
		"  ":                 nil,
	} {
		got := channelInfo(tuner.Channel{ID: "1", Name: "one", Group: group}).Tags
		if !slices.Equal(got, want) {
			t.Fatalf("group %q: tags %q, want %q", group, got, want)
		}
	}
}
