// SPDX-License-Identifier: GPL-2.0-only

package library

import "testing"

// TestIgnoredPathMatchesUpstreamTrickplayPatterns covers the "**/*.trickplay"
// and "**/*.trickplay/**" patterns of upstream IgnorePatterns.cs:72-74.
func TestIgnoredPathMatchesUpstreamTrickplayPatterns(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want bool
	}{
		{"/media/Movies/Film (2023)/Film (2023).trickplay", true},
		{"/media/Movies/Film (2023)/Film (2023).trickplay/320 - 10x10", true},
		{"/media/Movies/Film (2023)/Film (2023).trickplay/320 - 10x10/0.jpg", true},
		{"/media/TV/Show/Show - S01E01.TRICKPLAY", true},
		{`C:\media\TV\Show\Show - S01E01.trickplay\320 - 10x10`, true},
		{"/media/Movies/Film (2023)/Film (2023).mkv", false},
		{"/media/Movies/trickplay/Film.mkv", false},
		{"/media/Movies/Film.trickplay.mkv", false},
		{"/media/Movies/Film (2023)/Film (2023).trickplayer", false},
	} {
		if got := ignoredPath(testCase.path); got != testCase.want {
			t.Errorf("ignoredPath(%q) = %t, want %t", testCase.path, got, testCase.want)
		}
	}
}

func TestWithoutIgnoredEntriesKeepsInput(t *testing.T) {
	entries := []Entry{{Path: "/m/a.mkv"}, {Path: "/m/a.trickplay", IsDir: true}, {Path: "/m/b.mkv"}}
	kept := withoutIgnoredEntries(entries)
	if len(kept) != 2 || kept[0].Path != "/m/a.mkv" || kept[1].Path != "/m/b.mkv" {
		t.Fatalf("withoutIgnoredEntries = %#v, want the two videos", kept)
	}
	if entries[1].Path != "/m/a.trickplay" {
		t.Fatalf("withoutIgnoredEntries modified its input: %#v", entries)
	}
}
