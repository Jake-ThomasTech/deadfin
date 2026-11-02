// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
	"github.com/deadfin-org/deadfin/internal/store"
	"github.com/deadfin-org/deadfin/internal/store/sqlite"
)

// TestScannerKeepsFolderWithCaseCollidingEntries is the #702 regression. A
// case-sensitive filesystem (for example a ZFS dataset exported over NFS)
// can hold two names that differ only in case. Item IDs hash the lowercased
// path, so both names resolve to one ID. The folder plan failed validation
// on every scan, and the folder's whole subtree (an artist's discography, a
// season) stayed out of the library. The scan must keep the folder and its
// other children, and report the colliding entry as skipped.
//
// macOS volumes are case-insensitive, so the fixture adds the colliding
// entries through an injected directory listing.
func TestScannerKeepsFolderWithCaseCollidingEntries(t *testing.T) {
	for _, test := range []struct {
		name string
		// extra maps a directory (relative to the library root) to the
		// entries the listing adds to it.
		extra     map[string][]library.Entry
		wantAudio int
		wantSkip  string
	}{
		{
			name: "album",
			extra: map[string][]library.Entry{
				"B":     {{Path: "B/one", IsDir: true}},
				"B/one": {{Path: "B/one/09 - other.flac"}},
			},
			wantAudio: 18,
			wantSkip:  "B/one",
		},
		{
			name:      "track",
			extra:     map[string][]library.Entry{"B/One": {{Path: "B/One/01 - T.flac"}}},
			wantAudio: 18,
			wantSkip:  "B/One/01 - t.flac",
		},
		{
			name: "artist",
			extra: map[string][]library.Entry{
				".":   {{Path: "b", IsDir: true}},
				"b":   {{Path: "b/Z", IsDir: true}},
				"b/Z": {{Path: "b/Z/01 - z.flac"}},
			},
			wantAudio: 18,
			wantSkip:  "b",
		},
		{
			// NFC and NFD spellings are different byte sequences, so they
			// are different items, as upstream.
			name: "unicode normalization",
			extra: map[string][]library.Entry{
				".":              {{Path: "Beyoncé", IsDir: true}, {Path: "Beyoncé", IsDir: true}},
				"Beyoncé":        {{Path: "Beyoncé/Album", IsDir: true}},
				"Beyoncé/Album":  {{Path: "Beyoncé/Album/01 - a.flac"}},
				"Beyoncé":       {{Path: "Beyoncé/Album", IsDir: true}},
				"Beyoncé/Album": {{Path: "Beyoncé/Album/01 - a.flac"}},
			},
			wantAudio: 20,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stores, err := sqlite.Open(t.Context(), ":memory:")
			if err != nil {
				t.Fatalf("open stores: %v", err)
			}
			t.Cleanup(func() { _ = stores.Close() })
			items := store.NewItemStore(stores)
			music := filepath.Join(t.TempDir(), "music")
			for _, artist := range []string{"A", "B", "C"} {
				for _, album := range []string{"One", "Two"} {
					for number := 1; number <= 3; number++ {
						writeScannerFile(t, filepath.Join(music, artist, album, fmt.Sprintf("%02d - t.flac", number)), "audio")
					}
				}
			}
			extra := make(map[string][]library.Entry, len(test.extra))
			for directory, entries := range test.extra {
				absolute := make([]library.Entry, 0, len(entries))
				for _, entry := range entries {
					entry.Path = filepath.Join(music, entry.Path)
					absolute = append(absolute, entry)
				}
				extra[filepath.Join(music, directory)] = absolute
			}
			readDir := func(path string) ([]library.Entry, error) {
				var entries []library.Entry
				if _, err := os.Stat(path); err == nil {
					if entries, err = scannerReadDir(path); err != nil {
						return nil, err
					}
				}
				return append(entries, extra[path]...), nil
			}
			topology, err := library.NewTopology(t.TempDir(), store.NewLibraryStore(stores))
			if err != nil {
				t.Fatalf("open topology: %v", err)
			}
			t.Cleanup(func() { _ = topology.Close() })
			if _, err := topology.AddVirtualFolder(t.Context(), "Music", "music", library.LibraryOptions{PathInfos: []library.MediaPathInfo{{Path: music}}}); err != nil {
				t.Fatalf("add music library: %v", err)
			}
			for pass := 1; pass <= 2; pass++ {
				scanner := newScanner(t, topology, items, library.NewDefaultChain(naming.DefaultOptions()), library.ScannerOptions{Concurrency: 2, ReadDir: readDir})
				result, err := scanner.Scan(t.Context())
				if closeErr := scanner.Close(); closeErr != nil {
					t.Fatalf("close scanner: %v", closeErr)
				}
				if err != nil {
					t.Fatalf("scan %d: %v", pass, err)
				}
				if test.wantSkip == "" {
					if len(result.Errors) != 0 {
						t.Fatalf("scan %d errors = %v, want none", pass, result.Errors)
					}
				} else {
					skip := filepath.Join(music, test.wantSkip)
					if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "skip item "+skip+":") {
						t.Fatalf("scan %d errors = %v, want one skip of %q", pass, result.Errors, skip)
					}
					if !errors.Is(result.Errors[0], library.ErrItemSkipped) {
						t.Fatalf("scan %d error %v does not match library.ErrItemSkipped", pass, result.Errors[0])
					}
				}
				audio := 0
				for _, value := range scannerItems(t, items) {
					if value.Kind == item.KindAudio {
						audio++
					}
				}
				if audio != test.wantAudio {
					t.Fatalf("scan %d audio items = %d, want %d", pass, audio, test.wantAudio)
				}
			}
			if got := scannerItemAtPath(t, items, filepath.Join(music, "B", "Two", "03 - t.flac")); got.Kind != item.KindAudio {
				t.Fatalf("sibling track kind = %s, want Audio", got.Kind)
			}
		})
	}
}
