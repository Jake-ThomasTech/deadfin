// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

const burnInASS = "[Script Info]\nScriptType: v4.00+\n\n[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,line\n"

// burnInSource is an HLS-eligible media source on a real file, with an
// embedded ASS stream and font attachments, so that the extraction caches
// can pin it.
func burnInSource(t *testing.T) playback.MediaSourceInfo {
	t.Helper()
	media := filepath.Join(t.TempDir(), "Show S01E01.mkv")
	if err := os.WriteFile(media, []byte("matroska"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned, err := sourcefile.Open(media, sourcefile.Expected{})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := pinned.Fingerprint()
	_ = pinned.Close()
	size, inode := fingerprint.Size, int64(fingerprint.Ino)
	modifiedAt, changedAt := fingerprint.MTime, fingerprint.CTime

	source := progressiveCopySource(media)
	source.ID = extractSourceID
	source.FileFacts = playback.SourceFileFacts{Size: &size, ModifiedAt: &modifiedAt, Inode: &inode, ChangedAt: &changedAt}
	language := "eng"
	source.MediaStreams = append(source.MediaStreams,
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 2, Codec: "ass", Language: &language, SupportsExternalStream: true},
		playback.MediaStream{Type: playback.StreamTypeSubtitle, Index: 3, Codec: "ass", SupportsExternalStream: true},
	)
	source.MediaAttachments = []playback.MediaAttachment{
		{Index: 4, Codec: "ttf", FileName: "Komika.ttf"},
		{Index: 5, Codec: "mjpeg", FileName: "cover.jpg"},
	}
	return source
}

func burnInRequest() playback.HLSRequest {
	request := hlsTranscodeRequest()
	selected := 2
	delivery := playback.SubtitleEncode
	request.SubtitleStreamIndex = &selected
	request.SubtitleDeliveryMethod = &delivery
	return request
}

func burnInService(source playback.MediaSourceInfo, manager playback.HLSManager, extractor playback.MediaExtractor, cache string) *playback.HLSService {
	sources := &subtitleServiceSource{source: source}
	config := hlsServiceConfig(&progressiveFakeSource{source: source}, manager)
	config.Subtitles = playback.NewSubtitleService(playback.SubtitleServiceConfig{
		Sources: sources, Extractor: extractor, ExtractionPath: filepath.Join(cache, "subtitles"),
	})
	config.Attachments = playback.NewAttachmentService(playback.AttachmentServiceConfig{
		Sources: sources, Extractor: extractor, Path: filepath.Join(cache, "attachments"),
	})
	return playback.NewHLSService(config)
}

// libass must not demux the whole media file each time ffmpeg starts.
// The burn-in reads the cached extraction, pinned like an external file, and
// loads fonts from the attachment cache.
func TestHLSServiceBurnsEmbeddedASSFromExtractionCacheWithFontsDir(t *testing.T) {
	source := burnInSource(t)
	cache := t.TempDir()
	extractor := &fakeExtractor{content: map[string]string{".ass": burnInASS, "4": "\x00\x01\x00\x00font"}}
	manager := &hlsFakeManager{segment: "encoded segment"}
	service := burnInService(source, manager, extractor, cache)
	request := burnInRequest()

	for range 2 {
		if _, err := service.Segment(t.Context(), request); err != nil {
			t.Fatalf("Segment(): %v", err)
		}
	}
	// One subtitle pass and one attachment pass; the second request reuses both caches.
	if got := extractor.runCount(); got != 2 {
		t.Fatalf("extractor runs = %d (%q), want 2", got, extractor.runs)
	}
	subtitleFile := filepath.Join(cache, "subtitles", "0a", "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9", "2.ass")
	fonts := filepath.Join(cache, "attachments", "0a", "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9")
	if _, err := os.Stat(filepath.Join(fonts, "Komika.ttf")); err != nil {
		t.Fatalf("extracted font: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fonts, "cover.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cover art stat error = %v, want not extracted (mjpeg)", err)
	}

	job := manager.lastRestart
	if job.EmbeddedSubtitleSource != "" {
		t.Fatalf("EmbeddedSubtitleSource = %q, want empty: libass must not read the media file", job.EmbeddedSubtitleSource)
	}
	if want := "subtitles=f='" + subtitleFile + "'"; job.ExternalSubtitleSource != want {
		t.Fatalf("ExternalSubtitleSource = %q, want %q", job.ExternalSubtitleSource, want)
	}
	if job.ExternalSubtitleRevision == "" || job.OpenExternalSubtitle == nil {
		t.Fatalf("extracted subtitle is not pinned: revision %q, opener nil %t", job.ExternalSubtitleRevision, job.OpenExternalSubtitle == nil)
	}
	pinned, err := job.OpenExternalSubtitle()
	if err != nil {
		t.Fatalf("OpenExternalSubtitle(): %v", err)
	}
	if pinned.Path() != subtitleFile {
		t.Errorf("pinned subtitle path = %q, want %q", pinned.Path(), subtitleFile)
	}
	_ = pinned.Close()

	filter := hlsConstraintArgumentValue(job.Args, "-vf")
	if want := job.ExternalSubtitleSource + ":fontsdir='" + fonts + "'"; strings.Count(filter, job.ExternalSubtitleSource) != 1 || !strings.Contains(filter, want) {
		t.Fatalf("-vf = %q, want one %q", filter, want)
	}
	if strings.Contains(filter, source.Path) || strings.Contains(filter, ":si=") {
		t.Fatalf("-vf = %q, want no subtitle read from the media file", filter)
	}
}

// TestHLSServiceBurnsEmbeddedSubRipFromExtractionCache covers #860: a SubRip
// stream is extracted as SRT and burned from that file, so a start or a seek
// does not make libass demux the whole media file.
func TestHLSServiceBurnsEmbeddedSubRipFromExtractionCache(t *testing.T) {
	source := burnInSource(t)
	source.MediaStreams[2].Codec = "subrip"
	cache := t.TempDir()
	extractor := &fakeExtractor{content: map[string]string{".ass": burnInASS, ".srt": "1\n00:00:01,000 --> 00:00:02,000\nline\n", "4": "\x00\x01\x00\x00font"}}
	manager := &hlsFakeManager{segment: "encoded segment"}
	service := burnInService(source, manager, extractor, cache)

	for range 2 {
		if _, err := service.Segment(t.Context(), burnInRequest()); err != nil {
			t.Fatalf("Segment(): %v", err)
		}
	}
	if got := extractor.runCount(); got != 2 {
		t.Fatalf("extractor runs = %d (%q), want 2", got, extractor.runs)
	}
	subtitleFile := filepath.Join(cache, "subtitles", "0a", "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9", "2.srt")
	job := manager.lastRestart
	if want := "subtitles=f='" + subtitleFile + "'"; job.EmbeddedSubtitleSource != "" || job.ExternalSubtitleSource != want {
		t.Fatalf("subtitle sources = embedded %q external %q, want external %q", job.EmbeddedSubtitleSource, job.ExternalSubtitleSource, want)
	}
	if filter := hlsConstraintArgumentValue(job.Args, "-vf"); strings.Contains(filter, source.Path) || strings.Contains(filter, ":si=") {
		t.Fatalf("-vf = %q, want no subtitle read from the media file", filter)
	}
}

// D-61: a failed extraction does not fail the start; libass reads the media
// file as before.
func TestHLSServiceBurnsFromMediaFileWhenExtractionFails(t *testing.T) {
	source := burnInSource(t)
	extractor := &fakeExtractor{err: errors.New("ffmpeg exited 1")}
	manager := &hlsFakeManager{segment: "encoded segment"}
	service := burnInService(source, manager, extractor, t.TempDir())

	if _, err := service.Segment(t.Context(), burnInRequest()); err != nil {
		t.Fatalf("Segment(): %v", err)
	}
	job := manager.lastRestart
	if job.ExternalSubtitleSource != "" || job.EmbeddedSubtitleSource != "subtitles=f='"+source.Path+"'" {
		t.Fatalf("subtitle sources = embedded %q external %q, want the media file", job.EmbeddedSubtitleSource, job.ExternalSubtitleSource)
	}
	if filter := hlsConstraintArgumentValue(job.Args, "-vf"); !strings.Contains(filter, job.EmbeddedSubtitleSource+":si=0") || strings.Contains(filter, "fontsdir") {
		t.Fatalf("-vf = %q, want the media-file subtitle read without fontsdir", filter)
	}
}

func TestAttachmentServiceExtractAllDumpsMissingAttachmentsOnce(t *testing.T) {
	source, _ := extractionFixture(t)
	cache := t.TempDir()
	extractor := &fakeExtractor{content: map[string]string{"6": "font", "7": "OTTO", "9": "blob"}}
	service := playback.NewAttachmentService(playback.AttachmentServiceConfig{
		Sources: &subtitleServiceSource{source: source}, Extractor: extractor, Path: cache,
	})
	folder := filepath.Join(cache, "0a", "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	// A cached attachment is not dumped again.
	if err := os.WriteFile(filepath.Join(folder, "escape.otf"), []byte("OTTO"), 0o644); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		got, err := service.ExtractAll(t.Context(), source)
		if err != nil {
			t.Fatalf("ExtractAll(): %v", err)
		}
		if got != folder {
			t.Fatalf("ExtractAll() = %q, want %q", got, folder)
		}
	}
	want := strings.Join([]string{"-nostdin",
		"-dump_attachment:6", filepath.Join(folder, "Komika.ttf.part"),
		"-dump_attachment:9", filepath.Join(folder, "9.part"),
		"-i", "/dev/fd/3", "-t", "0", "-f", "null", "null"}, " ")
	if extractor.runCount() != 1 || strings.Join(extractor.runs[0], " ") != want {
		t.Fatalf("extractor runs = %q, want one run %q", extractor.runs, want)
	}
	for _, name := range []string{"Komika.ttf", "9", "escape.otf"} {
		if _, err := os.Stat(filepath.Join(folder, name)); err != nil {
			t.Errorf("attachment %s: %v", name, err)
		}
	}
	for _, name := range []string{"cover.jpg", "Komika.ttf.part", "9.part"} {
		if _, err := os.Stat(filepath.Join(folder, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stat error = %v, want not present", name, err)
		}
	}
}

func TestAttachmentServiceExtractAllFailureLeavesNoPartialFiles(t *testing.T) {
	source, _ := extractionFixture(t)
	cache := t.TempDir()
	service := playback.NewAttachmentService(playback.AttachmentServiceConfig{
		Sources: &subtitleServiceSource{source: source}, Extractor: &fakeExtractor{err: errors.New("boom")}, Path: cache,
	})
	if _, err := service.ExtractAll(t.Context(), source); err == nil {
		t.Fatal("ExtractAll() error = nil, want the extractor failure")
	}
	folder := filepath.Join(cache, "0a", "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9")
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("attachment folder holds %d entries after a failure, want none", len(entries))
	}
}
