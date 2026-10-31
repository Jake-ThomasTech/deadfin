// SPDX-License-Identifier: GPL-2.0-only

package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/item"
	"github.com/deadfin-org/deadfin/internal/library"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

// recordingImageExtractor writes a fixed image for each extraction and
// records the jobs.
type recordingImageExtractor struct {
	jobs []encoding.ImageExtraction
	err  error
}

func (r *recordingImageExtractor) Extract(_ context.Context, job encoding.ImageExtraction, _ *sourcefile.Source, _ time.Duration) error {
	r.jobs = append(r.jobs, job)
	if r.err != nil {
		return r.err
	}
	content := fmt.Sprintf("image:%s:%v", filepath.Ext(job.OutputPath), job.StreamIndex != nil)
	return os.WriteFile(job.OutputPath, []byte(content), 0o600)
}

func dynamicImageNames(providers []dynamicImageProvider) []string {
	names := make([]string, 0, len(providers))
	for _, provider := range providers {
		names = append(names, provider.Name())
	}
	return names
}

func TestDynamicImageProviderEnablementAndOrder(t *testing.T) {
	both := []string{EmbeddedImageExtractorName, ScreenGrabberName}
	tests := []struct {
		name     string
		options  library.LibraryOptions
		kind     item.Kind
		settings DynamicImageSettings
		want     []string
	}{
		{name: "no type options uses the default order", kind: item.KindMovie, want: both},
		{
			name:     "server disabled list without type options",
			kind:     item.KindMovie,
			settings: DynamicImageSettings{DisabledImageFetchers: []string{"screen grabber"}},
			want:     []string{EmbeddedImageExtractorName},
		},
		{
			name:     "server image fetcher order",
			kind:     item.KindMovie,
			settings: DynamicImageSettings{ImageFetcherOrder: []string{ScreenGrabberName, EmbeddedImageExtractorName}},
			want:     []string{ScreenGrabberName, EmbeddedImageExtractorName},
		},
		{
			name: "new library defaults enable only the Screen Grabber",
			kind: item.KindMovie,
			options: library.LibraryOptions{TypeOptions: []library.TypeOptions{{
				Type: "Movie", ImageFetchers: []string{ScreenGrabberName},
			}}},
			want: []string{ScreenGrabberName},
		},
		{
			name: "type options win over the server disabled list",
			kind: item.KindEpisode,
			options: library.LibraryOptions{TypeOptions: []library.TypeOptions{{
				Type: "episode", ImageFetchers: []string{ScreenGrabberName, EmbeddedImageExtractorName},
			}}},
			settings: DynamicImageSettings{DisabledImageFetchers: both},
			want:     both,
		},
		{
			name: "type options order wins over the server order",
			kind: item.KindMovie,
			options: library.LibraryOptions{TypeOptions: []library.TypeOptions{{
				Type:              "Movie",
				ImageFetchers:     both,
				ImageFetcherOrder: []string{ScreenGrabberName, EmbeddedImageExtractorName},
			}}},
			settings: DynamicImageSettings{ImageFetcherOrder: both},
			want:     []string{ScreenGrabberName, EmbeddedImageExtractorName},
		},
		{
			name: "type options with an empty fetcher list disable both",
			kind: item.KindMovie,
			options: library.LibraryOptions{TypeOptions: []library.TypeOptions{{
				Type: "Movie",
			}}},
			want: []string{},
		},
		{
			name: "type options for another kind fall back to the server",
			kind: item.KindEpisode,
			options: library.LibraryOptions{TypeOptions: []library.TypeOptions{{
				Type: "Movie",
			}}},
			want: both,
		},
		{name: "audio has only the Image Extractor", kind: item.KindAudio, want: []string{AudioImageExtractorName}},
		{
			name:     "server disabled list turns off the Image Extractor",
			kind:     item.KindAudio,
			settings: DynamicImageSettings{DisabledImageFetchers: []string{"image extractor"}},
			want:     []string{},
		},
	}
	providers := NewDynamicImages(DynamicImagesOptions{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := "/media/file"
			got := dynamicImageNames(providers.enabledProviders(test.options, item.Item{Kind: test.kind, Path: &path}, test.settings))
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("enabled providers = %q, want %q", got, test.want)
			}
		})
	}
}

func TestScreenGrabberPlan(t *testing.T) {
	path, container := "/media/movie.mkv", "mkv"
	ticks := int64(1_200 * time.Second / 100)
	dvd := item.VideoTypeDvd
	halfSBS := item.Video3DFormatHalfSideBySide
	hdr := "smpte2084"
	video := playback.MediaStream{Type: playback.StreamTypeVideo, Index: 0, Codec: "hevc", IsInterlaced: true, ColorTransfer: &hdr}
	audio := playback.MediaStream{Type: playback.StreamTypeAudio, Index: 1, Codec: "aac"}
	movie := item.Item{Kind: item.KindMovie, Path: &path, Container: &container, RunTimeTicks: &ticks, Video3DFormat: &halfSBS}

	plan, ok := screenGrabber{}.Plan(movie, item.ImageTypePrimary, []playback.MediaStream{audio, video}, nil)
	if !ok {
		t.Fatal("Screen Grabber has no image for a video with a video stream")
	}
	if plan.job.Offset == nil || *plan.job.Offset != 120*time.Second {
		t.Fatalf("offset = %v, want 10%% of the runtime", plan.job.Offset)
	}
	if plan.job.StreamIndex != nil || plan.job.Container != "mkv" || plan.extension != ".jpg" || !plan.job.Interlaced || !plan.job.HDR || plan.job.Video3D != encoding.Image3DHalfSideBySide {
		t.Fatalf("plan = %+v, want the default stream, mkv container, jpg, interlaced, HDR, half SBS", plan)
	}

	unknown := movie
	unknown.RunTimeTicks = nil
	if plan, _ := (screenGrabber{}).Plan(unknown, item.ImageTypePrimary, []playback.MediaStream{video}, nil); plan.job.Offset == nil || *plan.job.Offset != 10*time.Second {
		t.Fatalf("offset without a runtime = %v, want 10s", plan.job.Offset)
	}
	if _, ok := (screenGrabber{}).Plan(movie, item.ImageTypePrimary, []playback.MediaStream{audio}, nil); ok {
		t.Fatal("Screen Grabber planned an image for audio-only media")
	}
	disc := movie
	disc.VideoType = &dvd
	if _, ok := (screenGrabber{}).Plan(disc, item.ImageTypePrimary, []playback.MediaStream{video}, nil); ok {
		t.Fatal("Screen Grabber planned an image for a DVD")
	}
	placeholder := movie
	placeholder.IsPlaceHolder = true
	if _, ok := (screenGrabber{}).Plan(placeholder, item.ImageTypePrimary, []playback.MediaStream{video}, nil); ok {
		t.Fatal("Screen Grabber planned an image for a placeholder")
	}
}

func TestEmbeddedImageExtractorPlan(t *testing.T) {
	path, container := "/media/movie.mkv", "mkv"
	movie := item.Item{Kind: item.KindMovie, Path: &path, Container: &container}
	episode := movie
	episode.Kind = item.KindEpisode
	if got := (embeddedImageExtractor{}).SupportedImages(episode); !reflect.DeepEqual(got, []item.ImageType{item.ImageTypePrimary}) {
		t.Fatalf("episode supported images = %v, want Primary", got)
	}
	if got := (embeddedImageExtractor{}).SupportedImages(movie); !reflect.DeepEqual(got, []item.ImageType{item.ImageTypePrimary, item.ImageTypeBackdrop, item.ImageTypeLogo}) {
		t.Fatalf("movie supported images = %v, want Primary, Backdrop, Logo", got)
	}

	attachments := []playback.MediaAttachment{
		{Index: 3, FileName: "notes.txt"},
		{Index: 4, FileName: "Cover_Land.PNG"},
		{Index: 5, FileName: "fanart-background.webp", MimeType: "image/webp"},
	}
	plan, ok := embeddedImageExtractor{}.Plan(movie, item.ImageTypePrimary, nil, attachments)
	if !ok || plan.job.StreamIndex == nil || *plan.job.StreamIndex != 4 || plan.extension != ".jpg" || plan.job.Offset != nil {
		t.Fatalf("primary attachment plan = %+v, %v; want index 4 as jpg (the extension switch is case-sensitive)", plan, ok)
	}
	plan, ok = embeddedImageExtractor{}.Plan(movie, item.ImageTypeBackdrop, nil, attachments)
	if !ok || *plan.job.StreamIndex != 5 || plan.extension != ".webp" {
		t.Fatalf("backdrop attachment plan = %+v, %v; want index 5 as webp", plan, ok)
	}

	logo := "Logo"
	streams := []playback.MediaStream{
		{Type: playback.StreamTypeVideo, Index: 0, Codec: "h264"},
		{Type: playback.StreamTypeEmbeddedImage, Index: 1, Codec: "mjpeg"},
		{Type: playback.StreamTypeEmbeddedImage, Index: 2, Codec: "png", Comment: &logo},
	}
	plan, ok = embeddedImageExtractor{}.Plan(movie, item.ImageTypeLogo, streams, nil)
	if !ok || *plan.job.StreamIndex != 2 || plan.extension != ".png" {
		t.Fatalf("logo stream plan = %+v, %v; want index 2 as png", plan, ok)
	}
	plan, ok = embeddedImageExtractor{}.Plan(movie, item.ImageTypePrimary, streams, nil)
	if !ok || *plan.job.StreamIndex != 1 || plan.extension != ".jpg" {
		t.Fatalf("primary fallback plan = %+v, %v; want the first embedded image", plan, ok)
	}
	if _, ok := (embeddedImageExtractor{}).Plan(movie, item.ImageTypeBackdrop, streams, nil); ok {
		t.Fatal("backdrop planned without a matching comment")
	}
}

func TestAudioImageExtractorPlan(t *testing.T) {
	path := "/music/Album/01.flac"
	track := item.Item{Kind: item.KindAudio, Path: &path}
	if !(audioImageExtractor{}).Supports(track) {
		t.Fatal("Image Extractor does not support a track")
	}
	book := track
	book.Kind = item.KindAudioBook
	if !(audioImageExtractor{}).Supports(book) {
		t.Fatal("Image Extractor does not support an audiobook")
	}
	movie := track
	movie.Kind = item.KindMovie
	if (audioImageExtractor{}).Supports(movie) || (embeddedImageExtractor{}).Supports(track) || (screenGrabber{}).Supports(track) {
		t.Fatal("audio and video providers overlap")
	}
	if _, ok := (audioImageExtractor{}).Plan(track, item.ImageTypePrimary, []playback.MediaStream{{Type: playback.StreamTypeAudio, Index: 0, Codec: "flac"}}, nil); ok {
		t.Fatal("Image Extractor planned an image without an embedded image stream")
	}

	back, front, other := "Cover (back)", "Cover (front)", "Other"
	streams := []playback.MediaStream{
		{Type: playback.StreamTypeAudio, Index: 0, Codec: "flac"},
		{Type: playback.StreamTypeEmbeddedImage, Index: 1, Codec: "png", Comment: &other},
		{Type: playback.StreamTypeEmbeddedImage, Index: 2, Codec: "mjpeg", Comment: &back},
		{Type: playback.StreamTypeEmbeddedImage, Index: 3, Codec: "mjpeg", Comment: &front},
	}
	tests := []struct {
		name    string
		streams []playback.MediaStream
		want    int
	}{
		{name: "front wins", streams: streams, want: 3},
		{name: "then cover", streams: streams[:3], want: 2},
		{name: "then the first image", streams: streams[:2], want: 1},
	}
	for _, test := range tests {
		plan, ok := audioImageExtractor{}.Plan(track, item.ImageTypePrimary, test.streams, nil)
		if !ok || plan.job.StreamIndex == nil || *plan.job.StreamIndex != test.want || !plan.job.Audio || plan.job.Container != "" || plan.extension != ".jpg" {
			t.Fatalf("%s: plan = %+v, %v; want audio index %d as jpg", test.name, plan, ok, test.want)
		}
	}
}

func TestRefreshDynamicImagesSavesAndSkips(t *testing.T) {
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := t.TempDir()
	container := "mkv"
	video := playback.MediaStream{Type: playback.StreamTypeVideo, Index: 0, Codec: "h264"}
	cover := playback.MediaAttachment{Index: 2, FileName: "cover.jpg", MimeType: "image/jpeg"}
	next := 0
	newItem := func(kind item.Kind) item.Item {
		next++
		id, err := dto.ParseUUID(fmt.Sprintf("%08x-0000-4000-8000-000000000487", next))
		if err != nil {
			t.Fatal(err)
		}
		path := media
		return item.Item{ID: id, Kind: kind, Path: &path, Container: &container}
	}
	input := library.DynamicImageInput{Image: library.RefreshModeDefault, Streams: []playback.MediaStream{video}, Attachments: []playback.MediaAttachment{cover}}

	t.Run("embedded first then screen grabber skips the primary", func(t *testing.T) {
		extractor := &recordingImageExtractor{}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		if err := providers.RefreshDynamicImages(t.Context(), input, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 1 || extractor.jobs[0].StreamIndex == nil || *extractor.jobs[0].StreamIndex != 2 {
			t.Fatalf("jobs = %+v, want one attachment extraction", extractor.jobs)
		}
		if len(target.ImageInfos) != 1 || target.ImageInfos[0].Type != item.ImageTypePrimary {
			t.Fatalf("images = %+v, want one primary", target.ImageInfos)
		}
		id := target.ID.String()
		pattern := regexp.MustCompile(`^%MetadataPath%/library/` + id[:2] + `/` + id + `/poster-[0-9a-f]{8}\.jpg$`)
		if !pattern.MatchString(target.ImageInfos[0].Path) {
			t.Fatalf("image path = %q, want the D-44 internal metadata name", target.ImageInfos[0].Path)
		}
		expanded := filepath.Join(metadata, strings.TrimPrefix(target.ImageInfos[0].Path, "%MetadataPath%/"))
		if _, err := os.Stat(expanded); err != nil {
			t.Fatalf("saved image: %v", err)
		}
		entries, _ := os.ReadDir(filepath.Dir(expanded))
		if len(entries) != 1 {
			t.Fatalf("metadata directory holds %d entries, want only the image", len(entries))
		}

		// A second pass finds the image and extracts nothing.
		if err := providers.RefreshDynamicImages(t.Context(), input, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 1 {
			t.Fatalf("second pass ran %d more extractions, want none", len(extractor.jobs)-1)
		}
	})

	t.Run("screen grabber only with library type options", func(t *testing.T) {
		extractor := &recordingImageExtractor{}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		withOptions := input
		withOptions.Options = library.LibraryOptions{TypeOptions: []library.TypeOptions{{Type: "Movie", ImageFetchers: []string{ScreenGrabberName}}}}
		if err := providers.RefreshDynamicImages(t.Context(), withOptions, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 1 || extractor.jobs[0].StreamIndex != nil || extractor.jobs[0].Offset == nil {
			t.Fatalf("jobs = %+v, want one screen grab", extractor.jobs)
		}
	})

	t.Run("image limit zero skips the type", func(t *testing.T) {
		extractor := &recordingImageExtractor{}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		limited := input
		limited.Options = library.LibraryOptions{TypeOptions: []library.TypeOptions{{
			Type:          "Movie",
			ImageFetchers: []string{ScreenGrabberName, EmbeddedImageExtractorName},
			ImageOptions:  []library.ImageOption{{Type: "Primary", Limit: 0}},
		}}}
		if err := providers.RefreshDynamicImages(t.Context(), limited, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 0 {
			t.Fatalf("jobs = %+v, want none for a disabled primary", extractor.jobs)
		}
	})

	t.Run("locked items refresh only on a full refresh", func(t *testing.T) {
		extractor := &recordingImageExtractor{}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		target.IsLocked = true
		if err := providers.RefreshDynamicImages(t.Context(), input, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 0 {
			t.Fatalf("locked default refresh ran %d extractions", len(extractor.jobs))
		}
		full := input
		full.Image = library.RefreshModeFullRefresh
		if err := providers.RefreshDynamicImages(t.Context(), full, &target); err != nil {
			t.Fatal(err)
		}
		if len(extractor.jobs) != 1 {
			t.Fatalf("locked full refresh ran %d extractions, want 1", len(extractor.jobs))
		}
	})

	t.Run("replace all images replaces and removes the old file", func(t *testing.T) {
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: &recordingImageExtractor{}, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		if err := providers.RefreshDynamicImages(t.Context(), input, &target); err != nil {
			t.Fatal(err)
		}
		previous := filepath.Join(metadata, strings.TrimPrefix(target.ImageInfos[0].Path, "%MetadataPath%/"))
		grab := input
		grab.Image, grab.ReplaceAllImages = library.RefreshModeFullRefresh, true
		grab.Attachments = nil
		if err := providers.RefreshDynamicImages(t.Context(), grab, &target); err != nil {
			t.Fatal(err)
		}
		if len(target.ImageInfos) != 1 || target.ImageInfos[0].Path == "" {
			t.Fatalf("images = %+v, want the replaced primary", target.ImageInfos)
		}
		replaced := filepath.Join(metadata, strings.TrimPrefix(target.ImageInfos[0].Path, "%MetadataPath%/"))
		if replaced == previous {
			t.Fatalf("replacement kept the path %q", previous)
		}
		if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("replaced image file stat = %v, want removed", err)
		}
		if _, err := os.Stat(replaced); err != nil {
			t.Fatalf("replacement image: %v", err)
		}
	})

	t.Run("a provider failure is logged and does not fail the refresh", func(t *testing.T) {
		extractor := &recordingImageExtractor{err: errors.New("ffmpeg failed")}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		target := newItem(item.KindMovie)
		if err := providers.RefreshDynamicImages(t.Context(), input, &target); err != nil {
			t.Fatalf("refresh error = %v, want nil", err)
		}
		if len(extractor.jobs) != 2 || len(target.ImageInfos) != 0 {
			t.Fatalf("jobs = %d images = %+v, want both providers tried and no image", len(extractor.jobs), target.ImageInfos)
		}
	})

	t.Run("unsupported kinds and episode backdrops", func(t *testing.T) {
		extractor := &recordingImageExtractor{}
		providers := NewDynamicImages(DynamicImagesOptions{Extractor: extractor, MetadataPath: metadata})
		audio := newItem(item.KindAudio)
		if err := providers.RefreshDynamicImages(t.Context(), input, &audio); err != nil || len(extractor.jobs) != 0 {
			t.Fatalf("audio refresh = %v with %d jobs, want no work", err, len(extractor.jobs))
		}
		backdrop := input
		backdrop.Attachments = []playback.MediaAttachment{{Index: 2, FileName: "backdrop.jpg"}}
		episode := newItem(item.KindEpisode)
		episode.ImageInfos = []item.ImageInfo{{Type: item.ImageTypePrimary, Path: "/media/episode-thumb.jpg"}}
		if err := providers.RefreshDynamicImages(t.Context(), backdrop, &episode); err != nil || len(extractor.jobs) != 0 {
			t.Fatalf("episode refresh = %v with %d jobs, want no backdrop work", err, len(extractor.jobs))
		}
	})
}

func TestCopyFolderImage(t *testing.T) {
	metadata := t.TempDir()
	providers := NewDynamicImages(DynamicImagesOptions{MetadataPath: metadata})
	trackID, err := dto.ParseUUID("00000001-0000-4000-8000-000000000784")
	if err != nil {
		t.Fatal(err)
	}
	albumID, err := dto.ParseUUID("00000002-0000-4000-8000-000000000784")
	if err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join(internalMetadataRelative(trackID), "poster-0011.jpg")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(metadata, relative)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, relative), []byte("cover"), 0o600); err != nil {
		t.Fatal(err)
	}
	track := item.Item{ID: trackID, Kind: item.KindAudio, ImageInfos: []item.ImageInfo{{Type: item.ImageTypePrimary, Path: dynamicMetadataPathToken + "/" + filepath.ToSlash(relative)}}}

	for _, path := range []string{"https://example.invalid/cover.jpg", "/media/cover"} {
		remote := item.Item{ID: trackID, Kind: item.KindAudio, ImageInfos: []item.ImageInfo{{Type: item.ImageTypePrimary, Path: path}}}
		album := item.Item{ID: albumID, Kind: item.KindMusicAlbum}
		if copied, err := providers.CopyFolderImage(t.Context(), remote, &album, item.ImageTypePrimary); copied || err != nil || len(album.ImageInfos) != 0 {
			t.Fatalf("copy from %q = %v, %v, images %+v; want no copy", path, copied, err, album.ImageInfos)
		}
	}

	album := item.Item{ID: albumID, Kind: item.KindMusicAlbum}
	copied, err := providers.CopyFolderImage(t.Context(), track, &album, item.ImageTypePrimary)
	if err != nil || !copied || len(album.ImageInfos) != 1 {
		t.Fatalf("copy = %v, %v, images %+v; want one Primary image", copied, err, album.ImageInfos)
	}
	prefix := dynamicMetadataPathToken + "/" + filepath.ToSlash(internalMetadataRelative(albumID)) + "/poster-"
	if image := album.ImageInfos[0]; image.Type != item.ImageTypePrimary || !strings.HasPrefix(image.Path, prefix) || !strings.HasSuffix(image.Path, ".jpg") {
		t.Fatalf("album image = %+v, want %s<tag>.jpg", image, prefix)
	}
	data, err := os.ReadFile(filepath.Join(metadata, filepath.FromSlash(strings.TrimPrefix(album.ImageInfos[0].Path, dynamicMetadataPathToken+"/"))))
	if err != nil || string(data) != "cover" {
		t.Fatalf("album image file = %q, %v; want the track cover", data, err)
	}
}
