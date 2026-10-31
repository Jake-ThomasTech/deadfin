// SPDX-License-Identifier: GPL-2.0-only

package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/api/handlers"
	"github.com/deadfin-org/deadfin/internal/api/router"
	"github.com/deadfin-org/deadfin/internal/config"
	"github.com/deadfin-org/deadfin/internal/providers"
	"github.com/deadfin-org/deadfin/internal/providers/anilist"
	"github.com/deadfin-org/deadfin/internal/providers/audiodb"
	"github.com/deadfin-org/deadfin/internal/providers/musicbrainz"
	"github.com/deadfin-org/deadfin/internal/providers/tmdb"
)

// availableOptionsRouter registers the route over the provider set of the
// composition root, in the same order.
func availableOptionsRouter(metadataOptions []config.MetadataOptions) *router.Router {
	registry := providers.NewRegistry(
		providers.NewLocalImageProvider(),
		providers.NewNFOProvider(providers.NFOProviderOptions{}),
		musicbrainz.NewProvider(musicbrainz.Options{}),
		audiodb.NewProvider(audiodb.Options{}),
		tmdb.NewProvider(tmdb.Options{}),
		anilist.NewProvider(anilist.Options{}),
	)
	rt := router.New()
	catalog := providers.Catalog{
		Registry:      registry,
		ImageFetchers: []providers.ImageFetcherSource{providers.NewDynamicImages(providers.DynamicImagesOptions{})},
	}
	handlers.RegisterLibraryOptionsInfo(rt, catalog, func() []config.MetadataOptions { return metadataOptions })
	return rt
}

// summarizeAvailableOptions renders a response as one line per list, with
// "name" for DefaultEnabled true and "-name" for false.
func summarizeAvailableOptions(t *testing.T, body []byte) string {
	t.Helper()
	type option struct {
		Name           string
		DefaultEnabled bool
	}
	var result struct {
		MetadataSavers, MetadataReaders, SubtitleFetchers, LyricFetchers, MediaSegmentProviders []option
		TypeOptions                                                                             []struct {
			Type                                                  string
			MetadataFetchers, ImageFetchers, SimilarItemProviders []option
			SupportedImageTypes                                   []string
			DefaultImageOptions                                   []struct {
				Type     string
				Limit    int
				MinWidth int
			}
		}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode available options: %v; body=%s", err, body)
	}
	names := func(options []option) string {
		parts := make([]string, len(options))
		for index, option := range options {
			parts[index] = option.Name
			if !option.DefaultEnabled {
				parts[index] = "-" + option.Name
			}
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	var lines []string
	lines = append(lines, "savers "+names(result.MetadataSavers), "readers "+names(result.MetadataReaders),
		"subtitles "+names(result.SubtitleFetchers), "lyrics "+names(result.LyricFetchers), "segments "+names(result.MediaSegmentProviders))
	for _, typeOptions := range result.TypeOptions {
		images := make([]string, len(typeOptions.DefaultImageOptions))
		for index, image := range typeOptions.DefaultImageOptions {
			images[index] = fmt.Sprintf("%s:%d:%d", image.Type, image.Limit, image.MinWidth)
		}
		lines = append(lines, fmt.Sprintf("%s metadata %s images %s similar %s supported [%s] defaults [%s]",
			typeOptions.Type, names(typeOptions.MetadataFetchers), names(typeOptions.ImageFetchers),
			names(typeOptions.SimilarItemProviders), strings.Join(typeOptions.SupportedImageTypes, ","), strings.Join(images, ",")))
	}
	return strings.Join(lines, "\n")
}

const (
	availableOptionsLists = "savers []\nreaders [Nfo]\nsubtitles []\nlyrics []\nsegments []\n"
	movieDefaults         = "Backdrop:1:1280,Art:0:0,Disc:0:0,Primary:1:0,Banner:0:0,Thumb:1:0,Logo:1:0"
	seriesDefaults        = "Backdrop:1:1280,Art:0:0,Primary:1:0,Banner:1:0,Thumb:1:0,Logo:1:0"
	seasonDefaults        = "Backdrop:0:1280,Primary:1:0,Banner:0:0,Thumb:0:0"
	episodeDefaults       = "Backdrop:0:1280,Primary:1:0"
)

// TestLibraryAvailableOptionsShape compares GET /Libraries/AvailableOptions
// with LibraryController.GetLibraryOptionsInfo over Deadfin's providers for
// movies, tvshows, music, and mixed (null) libraries, new and existing. The
// dynamic image providers follow the remote image fetchers
// for Movie, Episode, and MusicVideo; a new library enables Screen Grabber
// but not Embedded Image Extractor, as IsImageFetcherEnabledByDefault does.
func TestLibraryAvailableOptionsShape(t *testing.T) {
	rt := availableOptionsRouter(config.DefaultServerConfiguration().MetadataOptions)
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "new movies",
			query: "LibraryContentType=movies&IsNewLibrary=true",
			want: availableOptionsLists +
				"Movie metadata [TheMovieDb] images [TheMovieDb,-Embedded Image Extractor,Screen Grabber] similar [Local Genre/Tag] supported [Primary,Backdrop,Logo,Thumb] defaults [" + movieDefaults + "]",
		},
		{
			// New TV libraries enable TheMovieDb only for Series metadata;
			// upstream expects TheTVDB for the rest.
			name:  "new tvshows",
			query: "LibraryContentType=tvshows&IsNewLibrary=true",
			want: availableOptionsLists +
				"Series metadata [TheMovieDb,-AniList] images [-TheMovieDb,-AniList] similar [Local Genre/Tag] supported [Primary,Backdrop,Logo,Thumb,Banner] defaults [" + seriesDefaults + "]\n" +
				"Season metadata [-TheMovieDb,-AniList] images [-TheMovieDb] similar [] supported [Primary] defaults [" + seasonDefaults + "]\n" +
				"Episode metadata [-TheMovieDb] images [-TheMovieDb,-Embedded Image Extractor,Screen Grabber] similar [] supported [Primary] defaults [" + episodeDefaults + "]",
		},
		{
			name:  "existing tvshows",
			query: "LibraryContentType=tvshows&IsNewLibrary=false",
			want: availableOptionsLists +
				"Series metadata [TheMovieDb,AniList] images [TheMovieDb,AniList] similar [Local Genre/Tag] supported [Primary,Backdrop,Logo,Thumb,Banner] defaults [" + seriesDefaults + "]\n" +
				"Season metadata [TheMovieDb,AniList] images [TheMovieDb] similar [] supported [Primary] defaults [" + seasonDefaults + "]\n" +
				"Episode metadata [TheMovieDb] images [TheMovieDb,Embedded Image Extractor,Screen Grabber] similar [] supported [Primary] defaults [" + episodeDefaults + "]",
		},
		{
			name:  "new music",
			query: "LibraryContentType=music&IsNewLibrary=true",
			want: availableOptionsLists +
				"MusicArtist metadata [MusicBrainz,TheAudioDB] images [TheAudioDB] similar [Local Genre/Tag] supported [Primary,Logo,Banner,Backdrop] defaults [Backdrop:1:1280,Banner:0:0,Art:0:0,Logo:1:0]\n" +
				"MusicAlbum metadata [MusicBrainz,TheAudioDB] images [TheAudioDB] similar [Local Genre/Tag] supported [Primary,Disc] defaults [Backdrop:0:1280,Disc:0:0]\n" +
				"Audio metadata [] images [Image Extractor] similar [Local Genre/Tag] supported [Primary] defaults []\n" +
				"MusicVideo metadata [] images [-Embedded Image Extractor,Screen Grabber] similar [] supported [Primary,Backdrop,Logo] defaults [" + movieDefaults + "]",
		},
		{
			// The server MetadataOptions defaults disable TheAudioDB metadata
			// for albums and artists.
			name:  "existing music",
			query: "LibraryContentType=music",
			want: availableOptionsLists +
				"MusicArtist metadata [MusicBrainz,-TheAudioDB] images [TheAudioDB] similar [Local Genre/Tag] supported [Primary,Logo,Banner,Backdrop] defaults [Backdrop:1:1280,Banner:0:0,Art:0:0,Logo:1:0]\n" +
				"MusicAlbum metadata [MusicBrainz,-TheAudioDB] images [TheAudioDB] similar [Local Genre/Tag] supported [Primary,Disc] defaults [Backdrop:0:1280,Disc:0:0]\n" +
				"Audio metadata [] images [Image Extractor] similar [Local Genre/Tag] supported [Primary] defaults []\n" +
				"MusicVideo metadata [] images [Embedded Image Extractor,Screen Grabber] similar [] supported [Primary,Backdrop,Logo] defaults [" + movieDefaults + "]",
		},
		{
			name:  "new mixed",
			query: "IsNewLibrary=true",
			want: availableOptionsLists +
				"Series metadata [TheMovieDb,-AniList] images [-TheMovieDb,-AniList] similar [Local Genre/Tag] supported [Primary,Backdrop,Logo,Thumb,Banner] defaults [" + seriesDefaults + "]\n" +
				"Season metadata [-TheMovieDb,-AniList] images [-TheMovieDb] similar [] supported [Primary] defaults [" + seasonDefaults + "]\n" +
				"Episode metadata [-TheMovieDb] images [-TheMovieDb,-Embedded Image Extractor,Screen Grabber] similar [] supported [Primary] defaults [" + episodeDefaults + "]\n" +
				"Movie metadata [TheMovieDb] images [TheMovieDb,-Embedded Image Extractor,Screen Grabber] similar [Local Genre/Tag] supported [Primary,Backdrop,Logo,Thumb] defaults [" + movieDefaults + "]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := libraryRequest(t, rt, http.MethodGet, "/Libraries/AvailableOptions?"+tc.query, "", "")
			assertLibraryStatus(t, response, http.StatusOK)
			if got := summarizeAvailableOptions(t, response.Body.Bytes()); got != tc.want {
				t.Fatalf("available options\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestLibraryAvailableOptionsWireNames pins the member names and order of
// LibraryOptionsResultDto and LibraryTypeOptionsDto.
func TestLibraryAvailableOptionsWireNames(t *testing.T) {
	response := libraryRequest(t, availableOptionsRouter(nil), http.MethodGet, "/Libraries/AvailableOptions?libraryContentType=Movies&isNewLibrary=true", "", "")
	assertLibraryStatus(t, response, http.StatusOK)
	want := `{"MetadataSavers":[],"MetadataReaders":[{"Name":"Nfo","DefaultEnabled":true}],"SubtitleFetchers":[],"LyricFetchers":[],"MediaSegmentProviders":[],` +
		`"TypeOptions":[{"Type":"Movie","MetadataFetchers":[{"Name":"TheMovieDb","DefaultEnabled":true}],` +
		`"ImageFetchers":[{"Name":"TheMovieDb","DefaultEnabled":true},{"Name":"Embedded Image Extractor","DefaultEnabled":false},{"Name":"Screen Grabber","DefaultEnabled":true}],` +
		`"SimilarItemProviders":[{"Name":"Local Genre/Tag","DefaultEnabled":true}],"SupportedImageTypes":["Primary","Backdrop","Logo","Thumb"],` +
		`"DefaultImageOptions":[{"Type":"Backdrop","Limit":1,"MinWidth":1280},{"Type":"Art","Limit":0,"MinWidth":0},{"Type":"Disc","Limit":0,"MinWidth":0},` +
		`{"Type":"Primary","Limit":1,"MinWidth":0},{"Type":"Banner","Limit":0,"MinWidth":0},{"Type":"Thumb","Limit":1,"MinWidth":0},{"Type":"Logo","Limit":1,"MinWidth":0}]}]}`
	if got := strings.TrimSpace(response.Body.String()); got != want {
		t.Fatalf("body\ngot:  %s\nwant: %s", got, want)
	}
}

// TestLibraryAvailableOptionsBinding follows NullableEnumModelBinder: an
// unparseable content type binds as null, a number binds as its enum value.
// IsNewLibrary is a plain bool, so an invalid value is a binding error.
func TestLibraryAvailableOptionsBinding(t *testing.T) {
	rt := availableOptionsRouter(nil)
	types := func(query string) string {
		t.Helper()
		response := libraryRequest(t, rt, http.MethodGet, "/Libraries/AvailableOptions?"+query, "", "")
		assertLibraryStatus(t, response, http.StatusOK)
		var result struct{ TypeOptions []struct{ Type string } }
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		names := make([]string, len(result.TypeOptions))
		for index, typeOptions := range result.TypeOptions {
			names[index] = typeOptions.Type
		}
		return strings.Join(names, ",")
	}
	for query, want := range map[string]string{
		"LibraryContentType=bogus":       "Series,Season,Episode,Movie",
		"LibraryContentType=":            "Series,Season,Episode,Movie",
		"LibraryContentType=1":           "Movie",
		"LibraryContentType=99":          "Series,Season,Episode,Movie",
		"LibraryContentType=BOXSETS":     "BoxSet",
		"LibraryContentType=playlists":   "Playlist",
		"LibraryContentType=books":       "Book,AudioBook",
		"LibraryContentType=homevideos":  "Video,Photo",
		"LibraryContentType=photos":      "Video,Photo",
		"LibraryContentType=musicvideos": "MusicVideo",
	} {
		if got := types(query); got != want {
			t.Errorf("%s types = %s, want %s", query, got, want)
		}
	}
	response := libraryRequest(t, rt, http.MethodGet, "/Libraries/AvailableOptions?IsNewLibrary=maybe", "", "")
	assertLibraryStatus(t, response, http.StatusBadRequest)
}
