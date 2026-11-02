// SPDX-License-Identifier: GPL-2.0-only

package library

import (
	"path/filepath"
	"strings"
)

// trickplaySuffix is the directory suffix Jellyfin gives trickplay output
// that it saves next to the media (PathManager.GetTrickplayDirectory with
// SaveTrickplayWithMedia).
const trickplaySuffix = ".trickplay"

// nasSystemDirs ports the Synology and QNAP patterns of upstream
// IgnorePatterns (Emby.Server.Implementations/Library/IgnorePatterns.cs:82-100).
// Each name appears there as "**/name" and "**/name/**", so a path is ignored
// when any of its components is one of these names, compared
// case-insensitively.
var nasSystemDirs = []string{
	"eaDir",
	"@eaDir",
	"#recycle",
	"@Recycle",
	".@__thumb",
	"$RECYCLE.BIN",
	"System Volume Information",
	".grab",
}

// ignoredPath ports two groups of upstream IgnorePatterns, evaluated
// case-insensitively: the trickplay patterns "**/*.trickplay" and
// "**/*.trickplay/**" (IgnorePatterns.cs:72-74), which ignore a path when any
// of its components ends in ".trickplay", and the NAS system directories in
// nasSystemDirs.
// CoreResolutionIgnoreRule.ShouldIgnore applies the patterns before
// resolution, and LibraryManager.ResolvePath also drops ignored entries from
// FileSystemChildren, so an ignored entry is neither an item nor a child that
// a resolver sees.
func ignoredPath(path string) bool {
	for _, component := range strings.FieldsFunc(path, isPathSeparator) {
		if len(component) >= len(trickplaySuffix) && strings.EqualFold(component[len(component)-len(trickplaySuffix):], trickplaySuffix) {
			return true
		}
		if isNASSystemDir(component) {
			return true
		}
	}
	return false
}

func isNASSystemDir(component string) bool {
	for _, name := range nasSystemDirs {
		if strings.EqualFold(component, name) {
			return true
		}
	}
	return false
}

func isPathSeparator(r rune) bool {
	return r == filepath.Separator || r == '/' || r == '\\'
}

// withoutIgnoredEntries returns entries without the ones that ignoredPath
// matches. It does not modify entries, which an injected ReadDir may share.
func withoutIgnoredEntries(entries []Entry) []Entry {
	kept, _ := filterEntries(entries, func(entry Entry) (bool, error) {
		return ignoredPath(entry.Path), nil
	})
	return kept
}

// filterEntries returns entries without the ones that drop reports. It
// returns entries itself when drop reports none, and it does not modify
// entries.
func filterEntries(entries []Entry, drop func(Entry) (bool, error)) ([]Entry, error) {
	var kept []Entry
	for index, entry := range entries {
		dropped, err := drop(entry)
		if err != nil {
			return nil, err
		}
		if !dropped {
			if kept != nil {
				kept = append(kept, entry)
			}
			continue
		}
		if kept == nil {
			kept = make([]Entry, index, len(entries)-1)
			copy(kept, entries[:index])
		}
	}
	if kept == nil {
		return entries, nil
	}
	return kept, nil
}

// readMediaDir lists a media directory and drops the entries that the
// upstream ignore rules exclude from resolution: the IgnorePatterns ports in
// ignoredPath and the .ignore files of dotIgnoreRule.
func (s *Scanner) readMediaDir(path string) ([]Entry, error) {
	entries, err := s.options.ReadDir(path)
	if err != nil {
		return nil, err
	}
	return s.dotIgnore.filter(withoutIgnoredEntries(entries))
}
