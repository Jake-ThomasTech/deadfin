// SPDX-License-Identifier: GPL-2.0-only

package tuner

import (
	"strings"

	"github.com/deadfin-org/deadfin/internal/config"
)

// GroupCount is one M3U group-title and the number of channels in it.
type GroupCount struct {
	Name  string
	Count int
}

// FilterGroups applies a tuner's IncludeGroups and ExcludeGroups to its
// channels (D-91). Group names match after trimming, ignoring case. With
// no lists it returns channels unchanged.
func FilterGroups(tuner config.TunerHostInfo, channels []Channel) []Channel {
	include, exclude := groupSet(tuner.IncludeGroups), groupSet(tuner.ExcludeGroups)
	if len(include) == 0 && len(exclude) == 0 {
		return channels
	}
	out := make([]Channel, 0, len(channels))
	for _, channel := range channels {
		key := groupKey(channel.Group)
		if _, ok := include[key]; len(include) > 0 && !ok {
			continue
		}
		if _, ok := exclude[key]; ok {
			continue
		}
		out = append(out, channel)
	}
	return out
}

// CountGroups counts channels per group-title, in playlist order of each
// group's first channel. Names that differ only in case or surrounding
// space count as one group under the first spelling.
func CountGroups(channels []Channel) []GroupCount {
	out := []GroupCount{}
	index := make(map[string]int)
	for _, channel := range channels {
		key := groupKey(channel.Group)
		if at, ok := index[key]; ok {
			out[at].Count++
			continue
		}
		index[key] = len(out)
		out = append(out, GroupCount{Name: strings.TrimSpace(channel.Group), Count: 1})
	}
	return out
}

func groupSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[groupKey(name)] = struct{}{}
	}
	return out
}

func groupKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
