// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
)

// TestScannerIgnoresNASSystemDirectories ports the Synology and QNAP patterns
// of upstream IgnorePatterns (IgnorePatterns.cs:82-100). A Synology share
// holds "@eaDir" directories at the library root and next to every video,
// with thumbnails and index files inside; none of them may become an item.
func TestScannerIgnoresNASSystemDirectories(t *testing.T) {
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	if err := harness.scanner.Close(); err != nil {
		t.Fatalf("close fixture scanner: %v", err)
	}
	harness.chain = library.NewDefaultChain(naming.DefaultOptions())
	harness.scanner = newScanner(t, harness.topology, harness.items, harness.chain, library.ScannerOptions{Concurrency: 1, ReadDir: scannerReadDir})

	movieDirectory := scannerPath(t, harness.source, "Feature (2023)")
	media := scannerPath(t, movieDirectory, "Feature (2023).mkv")
	writeScannerFile(t, media, "media")
	for _, path := range []string{
		scannerPath(t, scannerPath(t, harness.source, "@eaDir"), "SYNOINDEX_MEDIA_INFO"),
		scannerPath(t, scannerPath(t, scannerPath(t, movieDirectory, "@eaDir"), "Feature (2023).mkv"), "SYNOVIDEO_VIDEO_SCREENSHOT.jpg"),
		scannerPath(t, scannerPath(t, harness.source, "#recycle"), "Deleted (2020).mkv"),
		scannerPath(t, scannerPath(t, harness.source, ".@__thumb"), "Feature.jpg"),
	} {
		writeScannerFile(t, path, "nas")
	}

	result := scannerScan(t, harness.scanner, "scan NAS fixture")
	if len(result.Errors) != 0 {
		t.Fatalf("scan NAS fixture errors = %v, want none", result.Errors)
	}
	for _, candidate := range scannerItems(t, harness.items) {
		if candidate.Path == nil {
			continue
		}
		path := strings.ToLower(*candidate.Path)
		for _, name := range []string{"@eadir", "#recycle", ".@__thumb"} {
			if strings.Contains(path, name) {
				t.Fatalf("scanner indexed NAS system path %q as %s", *candidate.Path, candidate.Kind)
			}
		}
	}
	if found := scannerItemAtPath(t, harness.items, media); found.IsFolder {
		t.Fatalf("media %q resolved as folder %s, want a video item", media, found.Kind)
	}
}
