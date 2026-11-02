// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"os"
	"path/filepath"
	"testing"
)

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

// TestIgnoredPathMatchesUpstreamNASPatterns covers the Synology and QNAP
// patterns of upstream IgnorePatterns.cs:82-100.
func TestIgnoredPathMatchesUpstreamNASPatterns(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want bool
	}{
		{"/media/tv/@eaDir", true},
		{"/media/tv/@eaDir/Show/SYNOINDEX_MEDIA_INFO", true},
		{"/media/tv/Show/Season 01/@eaDir/Show - S01E01.mkv/SYNOVIDEO_VIDEO_SCREENSHOT.jpg", true},
		{"/media/movies/@EADIR", true},
		{"/media/movies/eaDir/Film.mkv", true},
		{"/media/movies/#recycle/Film (2023)/Film (2023).mkv", true},
		{"/media/movies/@Recycle/Film.mkv", true},
		{"/media/movies/.@__thumb/Film.jpg", true},
		{"/media/movies/$RECYCLE.BIN/Film.mkv", true},
		{"/media/movies/System Volume Information", true},
		{"/media/movies/.grab/Film.mkv", true},
		{`D:\media\tv\@eaDir\Show`, true},
		{"/media/movies/Film @eaDir (2023)/Film.mkv", false},
		{"/media/movies/eaDirector (2023)/eaDirector (2023).mkv", false},
		{"/media/movies/recycle/Film.mkv", false},
		{"/media/movies/Recycle (2019)/Recycle (2019).mkv", false},
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

// TestDotIgnoreRulesMatchUpstream ports the CheckIgnoreRules data of upstream
// DotIgnoreIgnoreRuleTest and adds gitignore cases that exercise each
// ReplacerStash step of the Ignore library.
func TestDotIgnoreRulesMatchUpstream(t *testing.T) {
	rule1 := "SPs"
	rule2 := "SPs\n!thebestshot.mkv"
	rule3 := "*.txt\n{\\colortbl;\\red255\\green255\\blue255;}\nvideos/\n\\invalid\\escape\\sequence\n*.mkv"
	rule4 := "{\\colortbl;\\red255\\green255\\blue255;}\n\\invalid\\escape\\sequence"
	for _, testCase := range []struct {
		rules string
		path  string
		want  bool
	}{
		{rule1, "f:/cd/sps/ffffff.mkv", true},
		{rule1, "cd/sps/ffffff.mkv", true},
		{rule1, "/cd/sps/ffffff.mkv", true},
		{rule2, "f:/cd/sps/ffffff.mkv", true},
		{rule2, "/cd/sps/ffffff.mkv", true},
		{rule2, "f:/cd/sps/thebestshot.mkv", false},
		{rule2, "cd/sps/thebestshot.mkv", false},
		{rule2, "/cd/sps/thebestshot.mkv", false},
		{rule3, "test.txt", true},
		{rule3, "videos/movie.mp4", true},
		{rule3, "movie.mkv", true},
		{rule3, "test.mp3", false},
		{rule4, "any-file.txt", true},
		{rule4, "any/path/to/file.mkv", true},
		{"# comment only", "/m/Film.mkv", false},
		{"Extras/", "/m/Film/Extras/", true},
		{"Extras/", "/m/Film/Extras", false},
		{"sample?.mkv", "/m/Film/sample1.mkv", true},
		{"sample?.mkv", "/m/Film/sample12.mkv", false},
		{"**/Featurettes", "/m/Film/Featurettes/a.mkv", true},
		{"Film/**/a.mkv", "Film/x/y/a.mkv", true},
		{"Film/**", "Film/x/a.mkv", true},
		// LeadingSlash and MiddleSlash anchor a pattern at the start of
		// the path, and upstream checks the full path, so an anchored
		// pattern does not match an absolute path.
		{"/m/Film/*.nfo", "/m/Film/movie.nfo", false},
		{"Film/a.mkv", "/m/Film/a.mkv", false},
		{"Film/a.mkv", "Film/a.mkv", true},
		// The library escapes only "." and "+"; other regular expression
		// characters keep their meaning.
		{"Film (2021).mkv", "/m/Film (2021).mkv", false},
		{"Film (2021).mkv", "/m/Film 2021.mkv", true},
		{"a+b.mkv", "/m/a+b.mkv", true},
		{"a.mkv", "/m/aXmkv", false},
	} {
		rules := parseDotIgnore(testCase.rules)
		got := rules.ignoreAll || rules.match(testCase.path)
		if got != testCase.want {
			t.Errorf("rules %q on %q = %t, want %t", testCase.rules, testCase.path, got, testCase.want)
		}
	}
}

// TestDotIgnoreRuleFindsNearestFile covers the directory walk of upstream
// FindIgnoreFileCached, the empty-file rule, and an entry for the directory
// that holds the file.
func TestDotIgnoreRuleFindsNearestFile(t *testing.T) {
	root := t.TempDir()
	writeDotIgnoreFixture(t, filepath.Join(root, ".ignore"), "*.tmp\n")
	writeDotIgnoreFixture(t, filepath.Join(root, "Film", "Plex Versions", ".ignore"), "")
	rule := newDotIgnoreRule()
	for _, testCase := range []struct {
		entry Entry
		want  bool
	}{
		{Entry{Path: filepath.Join(root, "Film", "Film.mkv")}, false},
		{Entry{Path: filepath.Join(root, "Film", "Film.tmp")}, true},
		{Entry{Path: filepath.Join(root, "Film", "Season 1", "a.tmp")}, true},
		{Entry{Path: filepath.Join(root, "Film", "Plex Versions"), IsDir: true}, true},
		{Entry{Path: filepath.Join(root, "Film", "Plex Versions", "Optimized for Mobile"), IsDir: true}, true},
		{Entry{Path: filepath.Join(root, "Film", "Plex Versions", "Optimized for Mobile", "Film.mp4")}, true},
		{Entry{Path: filepath.Join(root, "Film", "Plex Versions", "Film.mp4")}, true},
	} {
		got, err := rule.shouldIgnore(testCase.entry)
		if err != nil {
			t.Fatalf("shouldIgnore(%q): %v", testCase.entry.Path, err)
		}
		if got != testCase.want {
			t.Errorf("shouldIgnore(%q) = %t, want %t", testCase.entry.Path, got, testCase.want)
		}
	}
}

// TestDotIgnoreRuleResetSeesChanges defends the per-scan cache: after reset,
// the rule sees a .ignore file that was added or removed.
func TestDotIgnoreRuleResetSeesChanges(t *testing.T) {
	root := t.TempDir()
	entry := Entry{Path: filepath.Join(root, "Plex Versions"), IsDir: true}
	ignoreFile := filepath.Join(entry.Path, ".ignore")
	if err := os.MkdirAll(entry.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	rule := newDotIgnoreRule()
	assertDotIgnore(t, rule, entry, false)
	writeDotIgnoreFixture(t, ignoreFile, "")
	rule.reset()
	assertDotIgnore(t, rule, entry, true)
	if err := os.Remove(ignoreFile); err != nil {
		t.Fatal(err)
	}
	rule.reset()
	assertDotIgnore(t, rule, entry, false)
}

// TestDotIgnoreRuleDanglingLinkIgnoresAll covers ParseIgnoreFile for a link
// whose target is missing: upstream treats it as an empty file.
func TestDotIgnoreRuleDanglingLinkIgnoresAll(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, ".ignore")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	assertDotIgnore(t, nil, Entry{Path: filepath.Join(root, "Film.mkv")}, true)
}

func TestFilterEntriesReportsDotIgnoreReadError(t *testing.T) {
	root := t.TempDir()
	ignoreFile := filepath.Join(root, ".ignore")
	writeDotIgnoreFixture(t, ignoreFile, "*.tmp")
	if err := os.Chmod(ignoreFile, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(ignoreFile); err == nil {
		t.Skip("the file stays readable, for example as root")
	}
	if _, err := newDotIgnoreRule().filter([]Entry{{Path: filepath.Join(root, "Film.mkv")}}); err == nil {
		t.Fatal("filter with an unreadable .ignore returned no error")
	}
}

func assertDotIgnore(t *testing.T, rule *dotIgnoreRule, entry Entry, want bool) {
	t.Helper()
	got, err := rule.shouldIgnore(entry)
	if err != nil {
		t.Fatalf("shouldIgnore(%q): %v", entry.Path, err)
	}
	if got != want {
		t.Fatalf("shouldIgnore(%q) = %t, want %t", entry.Path, got, want)
	}
}

func writeDotIgnoreFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
