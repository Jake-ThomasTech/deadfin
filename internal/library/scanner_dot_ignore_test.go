// SPDX-License-Identifier: GPL-2.0-only

package library_test

import (
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/naming"
)

// dotIgnoreFixture is the library from issue #810: Plex keeps "Optimized for
// Mobile" copies in a "Plex Versions" directory inside each movie directory.
type dotIgnoreFixture struct {
	harness *scannerHarness
	movies  []string
	copies  []string
	plex    []string
}

func newDotIgnoreFixture(t *testing.T) *dotIgnoreFixture {
	t.Helper()
	harness := newScannerHarness(t, &scannerFixtureResolver{})
	if err := harness.scanner.Close(); err != nil {
		t.Fatalf("close fixture scanner: %v", err)
	}
	harness.chain = library.NewDefaultChain(naming.DefaultOptions())
	harness.scanner = newScanner(t, harness.topology, harness.items, harness.chain, library.ScannerOptions{Concurrency: 1, ReadDir: scannerReadDir})

	fixture := &dotIgnoreFixture{harness: harness}
	for _, name := range []string{"Encanto (2021)", "Raya and the Last Dragon (2021)"} {
		movieDirectory := scannerPath(t, harness.source, name+" {tmdb-568124}")
		plexDirectory := scannerPath(t, movieDirectory, "Plex Versions")
		fixture.movies = append(fixture.movies, scannerPath(t, movieDirectory, name+".mkv"))
		fixture.plex = append(fixture.plex, plexDirectory)
		fixture.copies = append(fixture.copies, scannerPath(t, plexDirectory, "Optimized for Mobile", name+".mp4"))
	}
	for index := range fixture.movies {
		writeScannerFile(t, fixture.movies[index], "media")
		writeScannerFile(t, fixture.copies[index], "copy")
	}
	return fixture
}

func (f *dotIgnoreFixture) scan(t *testing.T) {
	t.Helper()
	scannerScan(t, f.harness.scanner, "scan .ignore fixture")
}

func (f *dotIgnoreFixture) assertNoPlexItems(t *testing.T) {
	t.Helper()
	for _, candidate := range scannerItems(t, f.harness.items) {
		if candidate.Path != nil && strings.Contains(*candidate.Path, "Plex Versions") {
			t.Fatalf("scanner indexed ignored path %q as %s", *candidate.Path, candidate.Kind)
		}
	}
	f.assertMovies(t)
}

func (f *dotIgnoreFixture) assertMovies(t *testing.T) {
	t.Helper()
	for _, movie := range f.movies {
		if found := scannerItemAtPath(t, f.harness.items, movie); found.IsFolder {
			t.Fatalf("movie %q resolved as folder %s, want a video item", movie, found.Kind)
		}
	}
}

// TestScannerSkipsDirectoryWithEmptyDotIgnore ports upstream
// DotIgnoreIgnoreRule: an empty .ignore file ignores the directory that
// holds it and everything below it. Issue #810: the "Optimized for Mobile"
// copies under a "Plex Versions" directory with an empty .ignore must not
// become items, and items that an earlier scan stored there are removed.
func TestScannerSkipsDirectoryWithEmptyDotIgnore(t *testing.T) {
	fixture := newDotIgnoreFixture(t)
	fixture.scan(t)
	fixture.assertMovies(t)
	for _, copy := range fixture.copies {
		scannerItemAtPath(t, fixture.harness.items, copy)
	}

	for _, directory := range fixture.plex {
		writeScannerFile(t, scannerPath(t, directory, ".ignore"), "")
	}
	fixture.scan(t)
	fixture.assertNoPlexItems(t)
}

// TestScannerAppliesDotIgnorePatterns covers a non-empty .ignore file. Its
// lines are gitignore-style patterns that upstream matches against the full
// path of each entry below the directory that holds the file.
func TestScannerAppliesDotIgnorePatterns(t *testing.T) {
	fixture := newDotIgnoreFixture(t)
	writeScannerFile(t, scannerPath(t, fixture.harness.source, ".ignore"), "# Plex copies\nPlex Versions/\n")
	fixture.scan(t)
	fixture.assertNoPlexItems(t)
}

// TestScannerDotIgnoreNegatedPattern covers a negated pattern: it includes
// again a path that an earlier pattern ignores.
func TestScannerDotIgnoreNegatedPattern(t *testing.T) {
	fixture := newDotIgnoreFixture(t)
	writeScannerFile(t, scannerPath(t, fixture.harness.source, ".ignore"), "*.mp4\n!Encanto*.mp4\n")
	fixture.scan(t)
	fixture.assertMovies(t)
	scannerItemAtPath(t, fixture.harness.items, fixture.copies[0])
	assertScannerNoItemAtPath(t, fixture.harness.items, fixture.copies[1])
}
