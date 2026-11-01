// SPDX-License-Identifier: GPL-2.0-only

package nfo_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/metadata/nfo"
)

func TestParseMovieLedgerMapsMetadataWithoutExternalWork(t *testing.T) {
	got := parseFixture(t, item.KindMovie, "movie-ledger.nfo")
	patch := got.Patch

	assertString(t, "Name", patch.Name, "Justice League")
	assertString(t, "ForcedSortName", patch.ForcedSortName, "League, Justice")
	assertString(t, "OriginalTitle", patch.OriginalTitle, "Justice League")
	assertString(t, "Overview", patch.Overview, "United heroes.")
	assertString(t, "Tagline", patch.Tagline, "Justice for all.")
	assertString(t, "OfficialRating", patch.OfficialRating, "Australia:M")
	assertInt(t, "ProductionYear", patch.ProductionYear, 2017)
	assertInt64(t, "RunTimeTicks", patch.RunTimeTicks, 72_000_000_000)
	assertFloat32(t, "CommunityRating", patch.CommunityRating, 8.5)
	assertDate(t, "PremiereDate", patch.PremiereDate, "2017-11-15")
	assertDate(t, "EndDate", patch.EndDate, "2017-11-16")

	if patch.Kind != item.KindMovie {
		t.Fatalf("parsed movie kind = %s, want %s", patch.Kind, item.KindMovie)
	}
	if !patch.IsLocked {
		t.Fatal("parsed lockdata did not lock the item")
	}
	if want := []item.MetadataField{item.MetadataFieldGenres, item.MetadataFieldName}; !reflect.DeepEqual(patch.LockedFields, want) {
		t.Fatalf("parsed locked fields = %#v, want %#v; unknown locked fields must be ignored", patch.LockedFields, want)
	}
	if want := []string{"Action", "Adventure"}; !reflect.DeepEqual(got.Genres, want) {
		t.Fatalf("parsed genres = %#v, want %#v", got.Genres, want)
	}
	if want := []string{"DC Comics"}; !reflect.DeepEqual(got.Studios, want) {
		t.Fatalf("parsed studios = %#v, want %#v", got.Studios, want)
	}
	if want := []string{"superhero"}; !reflect.DeepEqual(got.Tags, want) {
		t.Fatalf("parsed tags = %#v, want %#v", got.Tags, want)
	}
	assertProviderID(t, got, "Imdb", "tt0974015")
	assertProviderID(t, got, "Tmdb", "141052")

	wantPeople := []nfo.Person{
		{Name: "Zack Snyder", Kind: item.PersonKindDirector},
		{Name: "Jerry Siegel", Kind: item.PersonKindWriter},
		{Name: "Joe Shuster", Kind: item.PersonKindWriter},
		{Name: "Ben Affleck", Role: stringPointer("Batman"), Kind: item.PersonKindActor, Order: intPointer(0)},
	}
	if !reflect.DeepEqual(got.People, wantPeople) {
		t.Fatalf("parsed people = %#v, want %#v", got.People, wantPeople)
	}
	if want := []item.MediaURL{{URL: stringPointer("https://www.youtube.com/watch?v=dQw4w9WgXcQ")}}; !reflect.DeepEqual(patch.Extra.RemoteTrailers, want) {
		t.Fatalf("parsed trailer URLs = %#v, want %#v", patch.Extra.RemoteTrailers, want)
	}
}

func TestParseTypeSpecificLedgerCases(t *testing.T) {
	for _, tt := range []struct {
		name   string
		kind   item.Kind
		file   string
		assert func(*testing.T, nfo.Result)
	}{
		{
			name: "series accepts trailing provider URL and maps TV facts",
			kind: item.KindSeries,
			file: "series-ledger.nfo",
			assert: func(t *testing.T, got nfo.Result) {
				patch := got.Patch
				assertString(t, "Name", patch.Name, "American Gods")
				assertString(t, "ForcedSortName", patch.ForcedSortName, "Gods, American")
				assertString(t, "Overview", patch.Overview, "Gods walk among us.")
				if patch.Extra.Series == nil {
					t.Fatal("series metadata did not initialize series fields")
				}
				assertString(t, "AirTime", patch.Extra.Series.AirTime, "21:00")
				if patch.SeriesStatus == nil || *patch.SeriesStatus != item.SeriesStatusContinuing {
					t.Fatalf("series status = %v, want %s", patch.SeriesStatus, item.SeriesStatusContinuing)
				}
				assertProviderID(t, got, "Tmdb", "46648")
				assertProviderID(t, got, "Tvdb", "253573")
				assertProviderID(t, got, "Imdb", "tt1898069")
			},
		},
		{
			name: "season maps explicit season fields",
			kind: item.KindSeason,
			file: "season-ledger.nfo",
			assert: func(t *testing.T, got nfo.Result) {
				assertString(t, "Name", got.Patch.Name, "Season One")
				assertString(t, "Overview", got.Patch.Overview, "The first season.")
				assertInt(t, "IndexNumber", got.Patch.IndexNumber, 1)
			},
		},
		{
			name: "multi episode concatenates parts and preserves episode range",
			kind: item.KindEpisode,
			file: "episode-ledger.nfo",
			assert: func(t *testing.T, got nfo.Result) {
				patch := got.Patch
				assertString(t, "Name", patch.Name, "Rising / Rising: Part Two")
				assertString(t, "OriginalTitle", patch.OriginalTitle, "Rising (1) / Rising (2)")
				assertString(t, "Overview", patch.Overview, "First part. / Second part.")
				assertString(t, "SeriesName", patch.SeriesName, "Stargate Atlantis")
				assertInt(t, "ParentIndexNumber", patch.ParentIndexNumber, 1)
				assertInt(t, "IndexNumber", patch.IndexNumber, 1)
				if patch.Extra.Episode == nil {
					t.Fatal("multi episode did not initialize episode fields")
				}
				assertInt(t, "IndexNumberEnd", patch.Extra.Episode.IndexNumberEnd, 2)
				assertDate(t, "PremiereDate", patch.PremiereDate, "2004-07-16")
			},
		},
		{
			name: "artist maps naming aliases and musicbrainz identifier",
			kind: item.KindMusicArtist,
			file: "artist-ledger.nfo",
			assert: func(t *testing.T, got nfo.Result) {
				assertString(t, "Name", got.Patch.Name, "U2")
				assertString(t, "SortName", got.Patch.SortName, "U2")
				assertString(t, "Overview", got.Patch.Overview, "Irish rock band")
				assertProviderID(t, got, "MusicBrainzArtist", "a3cb23fc-acd3-4ce0-8f36-1e5aa6a18432")
				if want := []string{"Rock"}; !reflect.DeepEqual(got.Genres, want) {
					t.Fatalf("artist genres = %#v, want %#v", got.Genres, want)
				}
				if want := []string{"Rock/Pop"}; !reflect.DeepEqual(got.Tags, want) {
					t.Fatalf("artist tags = %#v, want %#v", got.Tags, want)
				}
			},
		},
		{
			name: "album maps review release identifiers and credited artist",
			kind: item.KindMusicAlbum,
			file: "album-ledger.nfo",
			assert: func(t *testing.T, got nfo.Result) {
				assertString(t, "Name", got.Patch.Name, "The Best of 1980-1990")
				assertString(t, "Overview", got.Patch.Overview, "Greatest hits.")
				assertInt(t, "ProductionYear", got.Patch.ProductionYear, 1989)
				assertProviderID(t, got, "MusicBrainzAlbum", "59b5a40b-e2fd-3f18-a218-e8c9aae12ab5")
				assertProviderID(t, got, "MusicBrainzReleaseGroup", "6c301dbd-6ccb-3403-a6c4-6a22240a0297")
				if want := []nfo.Person{{Name: "U2", Kind: item.PersonKindAlbumArtist}}; !reflect.DeepEqual(got.People, want) {
					t.Fatalf("album artist credits = %#v, want %#v", got.People, want)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFixture(t, tt.kind, tt.file)
			if got.Patch.Kind != tt.kind {
				t.Fatalf("parsed %s kind = %s, want %s", tt.file, got.Patch.Kind, tt.kind)
			}
			tt.assert(t, got)
		})
	}
}

func TestParseToleratesUnknownAndMalformedTrailingElements(t *testing.T) {
	got, err := nfo.Parse(t.Context(), strings.NewReader("<movie>stray text<unknown><nested>ignored</nested></unknown><title>Known</title></movie>"), nfo.ParseOptions{Kind: item.KindMovie})
	if err != nil {
		t.Fatalf("Parse(valid NFO with unknown elements) error = %v", err)
	}
	assertString(t, "Name", got.Patch.Name, "Known")

	got, err = nfo.Parse(t.Context(), strings.NewReader("<movie><title>Known</title><unknown>broken"), nfo.ParseOptions{Kind: item.KindMovie})
	if err != nil {
		t.Fatalf("Parse(NFO with malformed trailing element) error = %v", err)
	}
	assertString(t, "Name", got.Patch.Name, "Known")
}

func TestParseHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := nfo.Parse(ctx, strings.NewReader("<movie><title>ignored</title></movie>"), nfo.ParseOptions{Kind: item.KindMovie})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Parse(cancelled context) error = %v, want context.Canceled", err)
	}
}

func parseFixture(t *testing.T, kind item.Kind, name string) nfo.Result {
	t.Helper()

	fixture, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture %q: %v", name, err)
	}
	t.Cleanup(func() { _ = fixture.Close() })

	got, err := nfo.Parse(t.Context(), fixture, nfo.ParseOptions{Kind: kind})
	if err != nil {
		t.Fatalf("Parse(%q): %v", name, err)
	}
	return got
}

func assertProviderID(t *testing.T, got nfo.Result, provider, want string) {
	t.Helper()
	actual, ok := got.ProviderIDs.Get(provider)
	if !ok || actual != want {
		t.Fatalf("provider ID %q = %q, %t; want %q, true", provider, actual, ok, want)
	}
}

func assertString(t *testing.T, field string, got *string, want string) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %q", field, got, want)
	}
}

func assertInt(t *testing.T, field string, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", field, got, want)
	}
}

func assertInt64(t *testing.T, field string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", field, got, want)
	}
}

func assertFloat32(t *testing.T, field string, got *float32, want float32) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %g", field, got, want)
	}
}

func assertDate(t *testing.T, field string, got *time.Time, want string) {
	t.Helper()
	if got == nil || got.Format(time.DateOnly) != want {
		t.Fatalf("%s = %v, want date %q", field, got, want)
	}
}

func stringPointer(value string) *string { return &value }
func intPointer(value int) *int          { return &value }
