// SPDX-License-Identifier: GPL-2.0-only

package tuner_test

import (
	"slices"
	"testing"

	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/livetv/tuner"
)

func TestFilterGroups(t *testing.T) {
	channels := []tuner.Channel{{ID: "a", Group: "News"}, {ID: "b", Group: " sports "}, {ID: "c"}, {ID: "d", Group: "FR | Kids"}}
	ids := func(list []tuner.Channel) []string {
		out := []string{}
		for _, channel := range list {
			out = append(out, channel.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name             string
		include, exclude []string
		want             []string
	}{
		{"none", nil, nil, []string{"a", "b", "c", "d"}},
		{"empty lists", []string{}, []string{}, []string{"a", "b", "c", "d"}},
		{"include", []string{"SPORTS", "fr | kids"}, nil, []string{"b", "d"}},
		{"exclude", nil, []string{"news ", ""}, []string{"b", "d"}},
		{"include then exclude", []string{"news", "sports"}, []string{"Sports"}, []string{"a"}},
	} {
		got := ids(tuner.FilterGroups(config.TunerHostInfo{IncludeGroups: tc.include, ExcludeGroups: tc.exclude}, channels))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCountGroups(t *testing.T) {
	got := tuner.CountGroups([]tuner.Channel{{Group: "News"}, {Group: "Sports"}, {Group: " news"}, {}, {Group: "Sports"}})
	want := []tuner.GroupCount{{Name: "News", Count: 2}, {Name: "Sports", Count: 2}, {Name: "", Count: 1}}
	if !slices.Equal(got, want) {
		t.Fatalf("%+v, want %+v", got, want)
	}
	if got := tuner.CountGroups(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
}
