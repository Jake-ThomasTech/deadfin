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

// ignoredPath ports the trickplay patterns of upstream IgnorePatterns
// (Emby.Server.Implementations/Library/IgnorePatterns.cs:72-74):
// "**/*.trickplay" and "**/*.trickplay/**", evaluated case-insensitively.
// A path is ignored when any of its components ends in ".trickplay".
// CoreResolutionIgnoreRule.ShouldIgnore applies the patterns before
// resolution, and LibraryManager.ResolvePath also drops ignored entries from
// FileSystemChildren, so an ignored entry is neither an item nor a child that
// a resolver sees.
func ignoredPath(path string) bool {
	for _, component := range strings.FieldsFunc(path, isPathSeparator) {
		if len(component) >= len(trickplaySuffix) && strings.EqualFold(component[len(component)-len(trickplaySuffix):], trickplaySuffix) {
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
	var kept []Entry
	for index, entry := range entries {
		if !ignoredPath(entry.Path) {
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
		return entries
	}
	return kept
}

// readMediaDir lists a media directory and drops the entries that the
// upstream ignore rules exclude from resolution.
func (s *Scanner) readMediaDir(path string) ([]Entry, error) {
	entries, err := s.options.ReadDir(path)
	if err != nil {
		return nil, err
	}
	return withoutIgnoredEntries(entries), nil
}
