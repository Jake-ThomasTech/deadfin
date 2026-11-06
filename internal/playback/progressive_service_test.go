// SPDX-License-Identifier: GPL-2.0-only

package playback_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/dto"
	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/playback"
	"github.com/deadfin-org/deadfin/internal/sourcefile"
)

func TestProgressiveServiceOpensExactCorpusCopyRemux(t *testing.T) {
	const mediaPath = "/media/goldenrec-input.mp4"

	for _, tc := range []struct {
		name, container, extension, contentType, identity string
		seek                                              int64
		args                                              []string
	}{
		{
			name:        "TS at start",
			container:   "ts",
			extension:   "ts",
			contentType: "video/mp2t",
			identity:    "3da0491805b51743169bd41513ba9c70",
			args: []string{
				"-fflags", "+genpts", "-i", "file:" + mediaPath, "-map", "0:0", "-map", "0:1", "-map", "-0:s",
				"-codec:v:0", "copy", "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0", "-codec:a:0", "copy",
			},
		},
		{
			name:        "TS at 90 seconds",
			container:   "ts",
			extension:   "ts",
			contentType: "video/mp2t",
			identity:    "c0d9944f5aca146c95eb3e33cf30311e",
			seek:        900_000_000,
			args: []string{
				"-ss", "00:01:30.000", "-fflags", "+genpts", "-i", "file:" + mediaPath, "-map", "0:0", "-map", "0:1", "-map", "-0:s",
				"-codec:v:0", "copy", "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0", "-codec:a:0", "copy",
			},
		},
		{
			name:        "fragmented MP4 at start",
			container:   "fmp4",
			extension:   "fmp4",
			contentType: "video/mp4",
			identity:    "3fe257632d7e878a2e2453ac7941935e",
			args: []string{
				"-fflags", "+genpts", "-i", "file:" + mediaPath, "-map", "0:0", "-map", "0:1", "-map", "-0:s",
				"-codec:v:0", "copy", "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0", "-codec:a:0", "copy",
				"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+delay_moov",
			},
		},
		{
			name:        "fragmented MP4 at 90 seconds",
			container:   "fmp4",
			extension:   "fmp4",
			contentType: "video/mp4",
			identity:    "c48b0cb53ccd47285f1a456a5f6c58ab",
			seek:        900_000_000,
			args: []string{
				"-ss", "00:01:30.000", "-fflags", "+genpts", "-i", "file:" + mediaPath, "-map", "0:0", "-map", "0:1", "-map", "-0:s",
				"-codec:v:0", "copy", "-map_metadata", "-1", "-map_chapters", "-1", "-threads", "0", "-codec:a:0", "copy",
				"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+delay_moov",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &progressiveFakeSource{source: progressiveCopySource(mediaPath)}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
				Sources:      source,
				TranscodeDir: "/cache",
				Opener:       opener,
			})

			request := progressiveCopyRequest()
			request.Container = tc.container
			request.StartTimeTicks = tc.seek
			response, err := service.OpenProgressive(t.Context(), request)
			if err != nil {
				t.Fatalf("OpenProgressive(): %v", err)
			}

			wantPath := "/cache/" + tc.identity + "." + tc.extension
			if response.OutputPath != wantPath {
				t.Errorf("OutputPath = %q, want exact MD5 identity %q", response.OutputPath, wantPath)
			}
			if response.ContentType != tc.contentType {
				t.Errorf("ContentType = %q, want %q", response.ContentType, tc.contentType)
			}
			if response.Body == nil {
				t.Fatal("Body = nil, want opener body")
			}
			if source.calls != 1 {
				t.Errorf("StaticSource calls = %d, want exactly one", source.calls)
			}
			if opener.calls != 1 {
				t.Fatalf("opener calls = %d, want one", opener.calls)
			}
			if opener.request.OutputPath != wantPath {
				t.Errorf("opener OutputPath = %q, want %q", opener.request.OutputPath, wantPath)
			}
			if opener.request.PlaySessionID != request.PlaySessionID || opener.request.DeviceID != request.DeviceID {
				t.Errorf("opener IDs = %#v, want play session %q and device %q", opener.request, request.PlaySessionID, request.DeviceID)
			}
			wantArgs := append(append([]string{}, tc.args...), "-y", wantPath)
			if difference := progressiveArgvDifference(wantArgs, opener.request.Args); difference != "" {
				t.Fatalf("copy-remux argv differs from committed progressive-remux corpus vector:\n%s", difference)
			}
		})
	}
}

func TestProgressiveServiceUsesStructuredRenditionIdentity(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/媒体/élan.mp4")}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
		Sources: source, TranscodeDir: "/cache",
		Opener: &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))},
	})
	output := func(request playback.ProgressiveRequest) string {
		t.Helper()
		response, err := service.OpenProgressive(t.Context(), request)
		if err != nil {
			t.Fatalf("OpenProgressive(): %v", err)
		}
		return response.OutputPath
	}

	first := progressiveCopyRequest()
	first.UserAgent, first.DeviceID, first.PlaySessionID = "a-b", "c", "session"
	second := first
	second.UserAgent, second.DeviceID = "a", "b-c"
	firstOutput, secondOutput := output(first), output(second)
	if firstOutput == secondOutput {
		t.Fatalf("delimiter-collision candidates shared output %q", firstOutput)
	}
	if got := output(first); got != firstOutput {
		t.Fatalf("identical request output = %q, want stable %q", got, firstOutput)
	}
}

func TestProgressiveServiceSeparatesSourceRevisions(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/revision-feature.mp4")}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
		Sources: source, TranscodeDir: "/cache",
		Opener: &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))},
	})
	request := progressiveCopyRequest()
	first, err := service.OpenProgressive(t.Context(), request)
	if err != nil {
		t.Fatalf("first OpenProgressive(): %v", err)
	}
	source.source.SourceRevision = "replacement-source-revision"
	second, err := service.OpenProgressive(t.Context(), request)
	if err != nil {
		t.Fatalf("replacement OpenProgressive(): %v", err)
	}
	if first.OutputPath == second.OutputPath {
		t.Fatalf("different source revisions shared output %q", first.OutputPath)
	}
}

func TestProgressiveServiceOpenerValidatesCanonicalSourceSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.mp4")
	const original = "original progressive source"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatalf("write progressive source: %v", err)
	}
	snapshot, err := sourcefile.Open(path, sourcefile.Expected{})
	if err != nil {
		t.Fatalf("snapshot progressive source: %v", err)
	}
	fingerprint := snapshot.Fingerprint()
	if err := snapshot.Close(); err != nil {
		t.Fatalf("close progressive snapshot: %v", err)
	}
	sourceInfo := progressiveCopySource(path)
	inode := int64(fingerprint.Ino)
	sourceInfo.SourceRevision = "canonical-progressive-source-revision"
	sourceInfo.FileFacts = playback.SourceFileFacts{
		Size:       &fingerprint.Size,
		ModifiedAt: &fingerprint.MTime,
		Inode:      &inode,
		ChangedAt:  &fingerprint.CTime,
	}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
		Sources: &progressiveFakeSource{source: sourceInfo}, TranscodeDir: "/cache", Opener: opener,
	})
	if _, err := service.OpenProgressive(t.Context(), progressiveCopyRequest()); err != nil {
		t.Fatalf("OpenProgressive(): %v", err)
	}
	if opener.request.SourceRevision != sourceInfo.SourceRevision || opener.request.OpenSource == nil {
		t.Fatalf("process source authority = revision:%q opener:%v", opener.request.SourceRevision, opener.request.OpenSource != nil)
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.mp4")
	if err := os.WriteFile(replacement, []byte(original), 0o600); err != nil {
		t.Fatalf("write replacement: %v", err)
	}
	if err := os.Chtimes(replacement, fingerprint.MTime, fingerprint.MTime); err != nil {
		t.Fatalf("preserve replacement mtime: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace source: %v", err)
	}
	if stale, openErr := opener.request.OpenSource(); openErr == nil {
		_ = stale.Close()
		t.Fatal("progressive source opener accepted an inode replacement")
	} else if !errors.Is(openErr, sourcefile.ErrStaleSource) {
		t.Fatalf("progressive source opener error = %v, want ErrStaleSource", openErr)
	}
}

func TestProgressiveServiceRejectsIncompleteCanonicalSourceAuthority(t *testing.T) {
	for _, field := range []string{"revision", "inode", "change time"} {
		t.Run(field, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/incomplete-authority.mp4")
			switch field {
			case "revision":
				sourceInfo.SourceRevision = ""
			case "inode":
				sourceInfo.FileFacts.Inode = nil
			case "change time":
				sourceInfo.FileFacts.ChangedAt = nil
			}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
				Sources: &progressiveFakeSource{source: sourceInfo}, TranscodeDir: "/cache", Opener: opener,
			})
			if _, err := service.OpenProgressive(t.Context(), progressiveCopyRequest()); !errors.Is(err, playback.ErrProgressiveUnsupported) {
				t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveUnsupported", err)
			}
			if opener.calls != 0 {
				t.Fatalf("opener calls = %d, want zero", opener.calls)
			}
		})
	}
}

func TestProgressiveServiceKeepsFmp4AndMP4OutputSuffixesDistinct(t *testing.T) {
	for _, tc := range []struct {
		container, suffix string
	}{
		{container: "fmp4", suffix: ".fmp4"},
		{container: "mp4", suffix: ".mp4"},
	} {
		t.Run(tc.container, func(t *testing.T) {
			source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
			request := progressiveCopyRequest()
			request.Container = tc.container

			response, err := service.OpenProgressive(t.Context(), request)
			if err != nil {
				t.Fatalf("OpenProgressive(): %v", err)
			}
			if !strings.HasSuffix(response.OutputPath, tc.suffix) {
				t.Errorf("OutputPath = %q, want suffix %q", response.OutputPath, tc.suffix)
			}
		})
	}
}

func TestProgressiveServiceSelectsSourceOnceAndPropagatesContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "request-context")
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
	request := progressiveCopyRequest()

	if _, err := service.OpenProgressive(ctx, request); err != nil {
		t.Fatalf("OpenProgressive(): %v", err)
	}
	if source.calls != 1 || source.userID != request.UserID || source.itemID != request.ItemID || source.mediaSourceID != request.MediaSourceID {
		t.Errorf("StaticSource call = calls:%d user:%s item:%s source:%q, want exactly request facts", source.calls, source.userID, source.itemID, source.mediaSourceID)
	}
	if source.ctx != ctx || opener.ctx != ctx {
		t.Error("service did not propagate caller context to both source resolution and opener")
	}
}

func TestProgressiveServiceHeadValidatesWithoutOpeningProcess(t *testing.T) {
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
	request := progressiveCopyRequest()
	request.Head = true

	response, err := service.OpenProgressive(t.Context(), request)
	if err != nil {
		t.Fatalf("OpenProgressive(HEAD): %v", err)
	}
	if response.Body != nil {
		t.Error("HEAD response Body is non-nil, want nil")
	}
	if response.ContentType != "video/mp2t" || response.OutputPath != "/cache/3da0491805b51743169bd41513ba9c70.ts" {
		t.Errorf("HEAD response metadata = %#v, want validated TS metadata", response)
	}
	if source.calls != 1 {
		t.Errorf("StaticSource calls = %d, want one even for HEAD", source.calls)
	}
	if opener.calls != 0 {
		t.Errorf("opener calls = %d, want no process for HEAD", opener.calls)
	}
}

func TestProgressiveServiceRejectsUnsupportedCopyRequestsWithoutOpening(t *testing.T) {
	selectedSubtitle := 2
	subtitleEncode := playback.SubtitleEncode
	maxWidth, maxHeight := 1279, 719
	maxFramerate := float32(24)
	videoBitrate, audioBitrate, maxChannels, sampleRate, bitDepth := 2_599_999, 383_999, 5, 47_999, 15
	videoOnly := progressiveCopySource("/media/goldenrec-input.mp4")
	videoOnly.MediaStreams = videoOnly.MediaStreams[:1]
	audioOnly := progressiveCopySource("/media/goldenrec-input.mp4")
	audioOnly.MediaStreams = audioOnly.MediaStreams[1:]
	remote := progressiveCopySource("/media/goldenrec-input.mp4")
	remote.IsRemote = true
	disc := progressiveCopySource("/media/goldenrec-input.mp4")
	discType := playback.VideoTypeDVD
	disc.VideoType = &discType

	for _, tc := range []struct {
		name    string
		request func(*playback.ProgressiveRequest)
		source  playback.MediaSourceInfo
	}{
		{name: "selected subtitle", request: func(r *playback.ProgressiveRequest) {
			r.SubtitleStreamIndex = &selectedSubtitle
			r.SubtitleDeliveryMethod = &subtitleEncode
		}},
		{name: "deinterlacing requested", request: func(r *playback.ProgressiveRequest) { r.DeInterlace = true }, source: progressiveInterlacedCopySource()},
		{name: "non-anamorphic required", request: func(r *playback.ProgressiveRequest) { r.RequireNonAnamorphic = true }, source: progressiveAnamorphicCopySource()},
		{name: "AVC required", request: func(r *playback.ProgressiveRequest) { r.RequireAvc = true }, source: progressiveNonAVCCopySource()},
		{name: "maximum width", request: func(r *playback.ProgressiveRequest) { r.MaxWidth = &maxWidth }},
		{name: "maximum height", request: func(r *playback.ProgressiveRequest) { r.MaxHeight = &maxHeight }},
		{name: "maximum framerate", request: func(r *playback.ProgressiveRequest) { r.MaxFramerate = &maxFramerate }},
		{name: "video bitrate", request: func(r *playback.ProgressiveRequest) { r.VideoBitrate = &videoBitrate }},
		{name: "audio bitrate", request: func(r *playback.ProgressiveRequest) { r.AudioBitrate = &audioBitrate }},
		{name: "audio channels", request: func(r *playback.ProgressiveRequest) { r.MaxAudioChannels = &maxChannels }},
		{name: "audio sample rate", request: func(r *playback.ProgressiveRequest) { r.AudioSampleRate = &sampleRate }},
		{name: "audio bit depth", request: func(r *playback.ProgressiveRequest) { r.MaxAudioBitDepth = &bitDepth }},
		{name: "missing selected audio", request: func(r *playback.ProgressiveRequest) { missing := 99; r.AudioStreamIndex = &missing }},
		{name: "missing video stream", request: func(r *playback.ProgressiveRequest) {}, source: audioOnly},
		{name: "missing audio stream", request: func(r *playback.ProgressiveRequest) {}, source: videoOnly},
		{name: "remote source", request: func(r *playback.ProgressiveRequest) {}, source: remote},
		{name: "disc source", request: func(r *playback.ProgressiveRequest) {}, source: disc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := tc.source
			if sourceInfo.Path == "" {
				sourceInfo = progressiveCopySource("/media/goldenrec-input.mp4")
			}
			source := &progressiveFakeSource{source: sourceInfo}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
			request := progressiveCopyRequest()
			tc.request(&request)

			_, err := service.OpenProgressive(t.Context(), request)
			if !errors.Is(err, playback.ErrProgressiveUnsupported) {
				t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveUnsupported", err)
			}
			if opener.calls != 0 {
				t.Errorf("opener calls = %d, want zero after unsupported copy request", opener.calls)
			}
		})
	}
}

func TestProgressiveServiceRequiresCopyCompatibleSelectedStreamsAndOutputCodecs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(*playback.ProgressiveRequest)
		source  func(*playback.MediaSourceInfo)
		wantErr error
	}{
		{name: "H264 AC3 source codecs in comma-separated target lists", request: func(request *playback.ProgressiveRequest) {
			request.VideoCodec, request.VideoCodecs = "h264,hevc", []string{"h264", "hevc"}
			request.AudioCodec, request.AudioCodecs = "ac3,aac", []string{"ac3", "aac"}
		}, source: func(*playback.MediaSourceInfo) {}},
		{name: "comma-separated target lists discard empty entries", request: func(request *playback.ProgressiveRequest) {
			request.VideoCodec, request.VideoCodecs = ",h264,,hevc,", []string{",h264,,hevc,"}
			request.AudioCodec, request.AudioCodecs = ",ac3,,aac,", []string{",ac3,,aac,"}
		}, source: func(*playback.MediaSourceInfo) {}},
		{name: "HEVC selected source", request: func(*playback.ProgressiveRequest) {}, source: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Codec = "hevc" }, wantErr: playback.ErrProgressiveUnsupported},
		{name: "AAC selected source", request: func(*playback.ProgressiveRequest) {}, source: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Codec = "aac" }, wantErr: playback.ErrProgressiveUnsupported},
		{name: "H264 absent from requested video codecs", request: func(request *playback.ProgressiveRequest) {
			request.VideoCodec, request.VideoCodecs = "hevc", []string{"hevc"}
		}, source: func(*playback.MediaSourceInfo) {}, wantErr: playback.ErrProgressiveUnsupported},
		{name: "AC3 absent from requested audio codecs", request: func(request *playback.ProgressiveRequest) {
			request.AudioCodec, request.AudioCodecs = "aac", []string{"aac"}
		}, source: func(*playback.MediaSourceInfo) {}, wantErr: playback.ErrProgressiveUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/goldenrec-input.mp4")
			tc.source(&sourceInfo)
			source := &progressiveFakeSource{source: sourceInfo}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
			request := progressiveCopyRequest()
			tc.request(&request)

			_, err := service.OpenProgressive(t.Context(), request)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("OpenProgressive(): %v", err)
				}
				if opener.calls != 1 {
					t.Errorf("opener calls = %d, want one", opener.calls)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("OpenProgressive() error = %v, want %v", err, tc.wantErr)
			}
			if opener.calls != 0 {
				t.Errorf("opener calls = %d, want zero", opener.calls)
			}
		})
	}
}

func TestProgressiveServiceHonorsCanStreamCopyGates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*playback.ProgressiveRequest)
		wantOpen  bool
	}{
		{name: "transport defaults allow copy", configure: func(*playback.ProgressiveRequest) {}, wantOpen: true},
		{name: "video copy disallowed", configure: func(request *playback.ProgressiveRequest) { request.AllowVideoStreamCopy = false }},
		{name: "audio copy disallowed", configure: func(request *playback.ProgressiveRequest) { request.AllowAudioStreamCopy = false }},
		{name: "automatic stream copy disabled", configure: func(request *playback.ProgressiveRequest) { request.EnableAutoStreamCopy = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpen(t, tc.configure)
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want successful one-process copy", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) {
				t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveUnsupported", err)
			}
			if opens != 0 {
				t.Errorf("opener calls = %d, want zero", opens)
			}
		})
	}
}

func TestProgressiveServiceHonorsH264CopyCapabilitiesAndQualifiedOptions(t *testing.T) {
	high, main := "High", "Main"
	level41, level40, invalidLevel := "4.1", "4.0", "not-a-number"
	refs4, refs3 := 4, 3
	depth8, depth7 := 8, 7
	for _, tc := range []struct {
		name      string
		configure func(*playback.ProgressiveRequest)
		wantOpen  bool
	}{
		{name: "exact generic source capabilities", configure: func(request *playback.ProgressiveRequest) {
			request.Profile, request.Level = &high, &level41
			request.MaxRefFrames, request.MaxVideoBitDepth = &refs4, &depth8
		}, wantOpen: true},
		{name: "invalid generic level is ignored", configure: func(request *playback.ProgressiveRequest) { request.Level = &invalidLevel }, wantOpen: true},
		{name: "global profile overrides restrictive h264 profile", configure: func(request *playback.ProgressiveRequest) {
			request.Profile = &high
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-profile", main)
		}, wantOpen: true},
		{name: "global level overrides restrictive h264 level", configure: func(request *playback.ProgressiveRequest) {
			request.Level = &level41
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}, wantOpen: true},
		{name: "global reference frame cap overrides restrictive h264 cap", configure: func(request *playback.ProgressiveRequest) {
			request.MaxRefFrames = &refs4
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-maxrefframes", "3")
		}, wantOpen: true},
		{name: "global video bit depth cap overrides restrictive h264 cap", configure: func(request *playback.ProgressiveRequest) {
			request.MaxVideoBitDepth = &depth8
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-videobitdepth", "7")
		}, wantOpen: true},
		{name: "global profile mismatch rejects matching h264 profile", configure: func(request *playback.ProgressiveRequest) {
			request.Profile = &main
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-profile", high)
		}},
		{name: "global level cap rejects matching h264 level", configure: func(request *playback.ProgressiveRequest) {
			request.Level = &level40
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level41)
		}},
		{name: "global reference frame cap rejects matching h264 cap", configure: func(request *playback.ProgressiveRequest) {
			request.MaxRefFrames = &refs3
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-maxrefframes", "4")
		}},
		{name: "global video bit depth cap rejects matching h264 cap", configure: func(request *playback.ProgressiveRequest) {
			request.MaxVideoBitDepth = &depth7
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-videobitdepth", "8")
		}},
		{name: "invalid nonempty global level suppresses qualified fallback", configure: func(request *playback.ProgressiveRequest) {
			request.Level = &invalidLevel
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}, wantOpen: true},
		{name: "empty global level falls back to restrictive h264 level", configure: func(request *playback.ProgressiveRequest) {
			emptyLevel := ""
			request.Level = &emptyLevel
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}},
		{name: "empty global profile falls back to restrictive h264 profile", configure: func(request *playback.ProgressiveRequest) {
			emptyProfile := ""
			request.Profile = &emptyProfile
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-profile", main)
		}},
		{name: "profile mismatch", configure: func(request *playback.ProgressiveRequest) { request.Profile = &main }},
		{name: "level exceeds cap", configure: func(request *playback.ProgressiveRequest) { request.Level = &level40 }},
		{name: "reference frames exceed cap", configure: func(request *playback.ProgressiveRequest) { request.MaxRefFrames = &refs3 }},
		{name: "video bit depth exceeds cap", configure: func(request *playback.ProgressiveRequest) { request.MaxVideoBitDepth = &depth7 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpen(t, tc.configure)
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want matching H264 copy", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported without process", err, opens)
			}
		})
	}
}

func TestProgressiveServiceNormalizesCopiedH264LevelsForCapabilities(t *testing.T) {
	level41, level40, invalidLevel := "4.1", "4.0", "not-a-number"
	source := progressiveCopySource("/media/goldenrec-input.mp4")
	if source.MediaStreams[0].Level == nil || *source.MediaStreams[0].Level != 41 {
		t.Fatalf("copied H264 source level = %v, want normalized 41", source.MediaStreams[0].Level)
	}

	for _, tc := range []struct {
		name      string
		configure func(*playback.ProgressiveRequest)
		wantOpen  bool
	}{
		{name: "decimal global level 4.1 accepts copied normalized level 41", configure: func(request *playback.ProgressiveRequest) { request.Level = &level41 }, wantOpen: true},
		{name: "decimal global level 4.0 rejects copied normalized level 41", configure: func(request *playback.ProgressiveRequest) { request.Level = &level40 }},
		{name: "decimal global level 4.1 overrides qualified decimal level 4.0", configure: func(request *playback.ProgressiveRequest) {
			request.Level = &level41
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}, wantOpen: true},
		{name: "invalid nonempty global level still suppresses qualified fallback", configure: func(request *playback.ProgressiveRequest) {
			request.Level = &invalidLevel
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}, wantOpen: true},
		{name: "empty global level falls back to qualified decimal level 4.0", configure: func(request *playback.ProgressiveRequest) {
			emptyLevel := ""
			request.Level = &emptyLevel
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-level", level40)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpen(t, tc.configure)
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want matching H264 copy", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported without process", err, opens)
			}
		})
	}
}

func TestProgressiveServiceHonorsH264CopyOptionGates(t *testing.T) {
	rotation90 := 90
	for _, tc := range []struct {
		name            string
		sourceConfigure func(*playback.MediaSourceInfo)
		configure       func(*playback.ProgressiveRequest)
		wantOpen        bool
	}{
		{name: "interlaced source permits explicit no deinterlace", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].IsInterlaced = true }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-deinterlace", "false")
		}, wantOpen: true},
		{name: "interlaced source rejects h264 deinterlace", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].IsInterlaced = true }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-deinterlace", "true")
		}},
		{name: "SDR source matches narrow h264 range type", sourceConfigure: func(*playback.MediaSourceInfo) {}, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rangetype", "SDR")
		}, wantOpen: true},
		{name: "matching global rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("rotation", "0,90")
		}, wantOpen: true},
		{name: "nonmatching global rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("rotation", "0,180")
		}},
		{name: "matching h264 rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "0,90")
		}, wantOpen: true},
		{name: "nonmatching h264 rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "0,180")
		}},
		{name: "global rotation overrides restrictive h264 rotation", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.Rotation = &rotation90
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "0")
		}, wantOpen: true},
		{name: "unrotated nil source ignores excluding h264 rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = nil }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "90")
		}, wantOpen: true},
		{name: "unrotated zero source ignores excluding h264 rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { zero := 0; source.MediaStreams[0].Rotation = &zero }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "90")
		}, wantOpen: true},
		{name: "nonzero source accepts pipe-separated h264 rotation list", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "0|90")
		}, wantOpen: true},
		{name: "global rotation rejects matching h264 rotation", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			zero := 0
			request.Rotation = &zero
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("h264-rotation", "90")
		}},
		{name: "empty global rotation option falls back to h264 rotation", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Rotation = &rotation90 }, configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("rotation", "")
			request.StreamOptions.Set("h264-rotation", "0")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpenWithSource(t, tc.sourceConfigure, tc.configure)
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want matching H264 option copy", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported without process", err, opens)
			}
		})
	}
}

func TestProgressiveServiceHonorsAC3CopyCapabilityPrecedence(t *testing.T) {
	channels6, channels5, channels4, channels3 := 6, 5, 4, 3
	depth16, depth15 := 16, 15
	qualifiedChannelsMatch := func(request *playback.ProgressiveRequest) {
		request.MaxAudioChannels, request.AudioChannels, request.TranscodingMaxAudioChannels = &channels5, &channels4, &channels3
		request.StreamOptions = playback.NewStreamOptions()
		request.StreamOptions.Set("ac3-audiochannels", "6")
	}
	for _, tc := range []struct {
		name      string
		configure func(*playback.ProgressiveRequest)
		wantOpen  bool
	}{
		{name: "qualified AC3 channel limit overrides generic channel limits", configure: qualifiedChannelsMatch, wantOpen: true},
		{name: "MaxAudioChannels precedes AudioChannels and transcoding default", configure: func(request *playback.ProgressiveRequest) {
			request.MaxAudioChannels, request.AudioChannels, request.TranscodingMaxAudioChannels = &channels6, &channels5, &channels4
		}, wantOpen: true},
		{name: "AudioChannels precedes transcoding default", configure: func(request *playback.ProgressiveRequest) {
			request.AudioChannels, request.TranscodingMaxAudioChannels = &channels6, &channels5
		}, wantOpen: true},
		{name: "transcoding channel default accepts exact source", configure: func(request *playback.ProgressiveRequest) { request.TranscodingMaxAudioChannels = &channels6 }, wantOpen: true},
		{name: "qualified audio channels exceeded", configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("ac3-audiochannels", "5")
		}},
		{name: "MaxAudioChannels exceeded", configure: func(request *playback.ProgressiveRequest) { request.MaxAudioChannels = &channels5 }},
		{name: "AudioChannels exceeded", configure: func(request *playback.ProgressiveRequest) { request.AudioChannels = &channels5 }},
		{name: "TranscodingMaxAudioChannels exceeded", configure: func(request *playback.ProgressiveRequest) { request.TranscodingMaxAudioChannels = &channels5 }},
		{name: "qualified audio bit depth exceeded", configure: func(request *playback.ProgressiveRequest) {
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("ac3-audiobitdepth", "15")
		}},
		{name: "MaxAudioBitDepth exceeded", configure: func(request *playback.ProgressiveRequest) { request.MaxAudioBitDepth = &depth15 }},
		{name: "exact generic audio bit depth", configure: func(request *playback.ProgressiveRequest) { request.MaxAudioBitDepth = &depth16 }, wantOpen: true},
		{name: "global audio bit depth overrides matching qualified AC3 limit", configure: func(request *playback.ProgressiveRequest) {
			request.MaxAudioBitDepth = &depth15
			request.StreamOptions = playback.NewStreamOptions()
			request.StreamOptions.Set("ac3-audiobitdepth", "16")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpen(t, tc.configure)
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want matching AC3 copy", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported without process", err, opens)
			}
		})
	}
}

func TestProgressiveServiceRanksH264Profiles(t *testing.T) {
	for _, tc := range []struct {
		name, sourceProfile, requested string
		wantOpen                       bool
	}{
		{name: "Main source is compatible with High request", sourceProfile: "Main", requested: "High", wantOpen: true},
		{name: "High 10 source exactly matches normalized High10 request", sourceProfile: "High 10", requested: "High10", wantOpen: true},
		{name: "known source must not exceed first requested score", sourceProfile: "High", requested: "Main,ProgressiveHigh"},
		{name: "first requested score permits lower source", sourceProfile: "High", requested: "ProgressiveHigh,Main", wantOpen: true},
		{name: "unknown source profile is rejected under a requested profile", sourceProfile: "vendor-private", requested: "High"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpenWithSource(t, func(source *playback.MediaSourceInfo) { source.MediaStreams[0].Profile = &tc.sourceProfile }, func(request *playback.ProgressiveRequest) { request.Profile = &tc.requested })
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want compatible H264 profile", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported profile", err, opens)
			}
		})
	}
}

func TestProgressiveServiceRequiresPositiveAC3ChannelFactsUnderACap(t *testing.T) {
	channels6, channels7, channels0, channelsNegative := 6, 7, 0, -1
	for _, tc := range []struct {
		name            string
		sourceConfigure func(*playback.MediaSourceInfo)
		wantOpen        bool
	}{
		{name: "exact channel cap", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Channels = &channels6 }, wantOpen: true},
		{name: "channel count above cap", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Channels = &channels7 }},
		{name: "nil channel count", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Channels = nil }},
		{name: "zero channel count", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Channels = &channels0 }},
		{name: "negative channel count", sourceConfigure: func(source *playback.MediaSourceInfo) { source.MediaStreams[1].Channels = &channelsNegative }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpenWithSource(t, tc.sourceConfigure, func(request *playback.ProgressiveRequest) { request.MaxAudioChannels = &channels6 })
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want exact positive channel cap", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported channel fact", err, opens)
			}
		})
	}
}

func TestProgressiveServiceHonorsJellyfinVideoRangeFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sourceConfigure func(*playback.MediaSourceInfo)
		requested       string
		wantOpen        bool
	}{
		{name: "SDR source accepts HDR10 request", sourceConfigure: func(*playback.MediaSourceInfo) {}, requested: "HDR10", wantOpen: true},
		{name: "pipe-separated range list accepts SDR source", sourceConfigure: func(*playback.MediaSourceInfo) {}, requested: "HDR10|SDR", wantOpen: true},
		{name: "SDR-only request rejects HDR10 source", sourceConfigure: func(source *playback.MediaSourceInfo) {
			transfer := "smpte2084"
			source.MediaStreams[0].ColorTransfer = &transfer
		}, requested: "SDR"},
		{name: "HDR10Plus source accepts HDR10 request", sourceConfigure: func(source *playback.MediaSourceInfo) {
			transfer := "smpte2084"
			plus := true
			source.MediaStreams[0].ColorTransfer, source.MediaStreams[0].Hdr10PlusPresentFlag = &transfer, &plus
		}, requested: "HDR10", wantOpen: true},
		{name: "DOVI with HDR10 source accepts HDR10 request", sourceConfigure: func(source *playback.MediaSourceInfo) {
			profile, rpu, bl, compatibility := 8, 1, 1, 1
			source.MediaStreams[0].DvProfile, source.MediaStreams[0].RpuPresentFlag, source.MediaStreams[0].BlPresentFlag, source.MediaStreams[0].DvBlSignalCompatibilityID = &profile, &rpu, &bl, &compatibility
		}, requested: "HDR10", wantOpen: true},
		{name: "plain DOVI source rejects request without DOVI", sourceConfigure: func(source *playback.MediaSourceInfo) {
			profile, rpu, bl, compatibility := 5, 1, 1, 0
			source.MediaStreams[0].DvProfile, source.MediaStreams[0].RpuPresentFlag, source.MediaStreams[0].BlPresentFlag, source.MediaStreams[0].DvBlSignalCompatibilityID = &profile, &rpu, &bl, &compatibility
		}, requested: "HDR10"},
		{name: "exact HDR10 source request", sourceConfigure: func(source *playback.MediaSourceInfo) {
			transfer := "smpte2084"
			source.MediaStreams[0].ColorTransfer = &transfer
		}, requested: "HDR10", wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opens, err := progressiveCopyOpenWithSource(t, tc.sourceConfigure, func(request *playback.ProgressiveRequest) {
				request.StreamOptions = playback.NewStreamOptions()
				request.StreamOptions.Set("h264-rangetype", tc.requested)
			})
			if tc.wantOpen {
				if err != nil || opens != 1 {
					t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want compatible range", err, opens)
				}
				return
			}
			if !errors.Is(err, playback.ErrProgressiveUnsupported) || opens != 0 {
				t.Fatalf("OpenProgressive() error = %v, opener calls = %d; want unsupported range", err, opens)
			}
		})
	}
}

func TestProgressiveServiceAllowsUnselectedEncodedSubtitles(t *testing.T) {
	subtitleEncode := playback.SubtitleEncode
	minusOne := -1
	for _, tc := range []struct {
		name  string
		index *int
	}{
		{name: "nil subtitle index", index: nil},
		{name: "minus one subtitle index", index: &minusOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
			request := progressiveCopyRequest()
			request.SubtitleDeliveryMethod = &subtitleEncode
			request.SubtitleStreamIndex = tc.index

			if _, err := service.OpenProgressive(t.Context(), request); err != nil {
				t.Fatalf("OpenProgressive(): %v", err)
			}
			if opener.calls != 1 {
				t.Errorf("opener calls = %d, want one", opener.calls)
			}
		})
	}
}

func TestProgressiveServiceRejectsSelectedEncodedSubtitles(t *testing.T) {
	subtitleEncode := playback.SubtitleEncode
	selected := 0
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
	request := progressiveCopyRequest()
	request.SubtitleDeliveryMethod = &subtitleEncode
	request.SubtitleStreamIndex = &selected

	_, err := service.OpenProgressive(t.Context(), request)
	if !errors.Is(err, playback.ErrProgressiveUnsupported) {
		t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveUnsupported", err)
	}
	if opener.calls != 0 {
		t.Errorf("opener calls = %d, want zero", opener.calls)
	}
}

func TestProgressiveServiceRejectsInvalidRequestsWithoutOpening(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(*playback.ProgressiveRequest)
		source  playback.MediaSourceInfo
	}{
		{name: "unsupported output container", request: func(r *playback.ProgressiveRequest) { r.Container = "avi" }},
		{name: "missing play session", request: func(r *playback.ProgressiveRequest) { r.PlaySessionID = "" }},
		{name: "empty source path", request: func(r *playback.ProgressiveRequest) {}, source: progressiveCopySource("")},
		{name: "unsafe source path", request: func(r *playback.ProgressiveRequest) {}, source: progressiveCopySource("-not-a-local-path")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := tc.source
			if sourceInfo.Path == "" && tc.name != "empty source path" {
				sourceInfo = progressiveCopySource("/media/goldenrec-input.mp4")
			}
			source := &progressiveFakeSource{source: sourceInfo}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
			request := progressiveCopyRequest()
			tc.request(&request)

			_, err := service.OpenProgressive(t.Context(), request)
			if !errors.Is(err, playback.ErrProgressiveInvalid) {
				t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveInvalid", err)
			}
			if opener.calls != 0 {
				t.Errorf("opener calls = %d, want zero after invalid request", opener.calls)
			}
		})
	}
}

func TestProgressiveServicePreservesSourceAndProcessErrors(t *testing.T) {
	sourceErr := errors.New("source unavailable")
	openerErr := errors.New("process spawn failed")
	for _, tc := range []struct {
		name   string
		source *progressiveFakeSource
		opener *progressiveFakeOpener
		want   error
	}{
		{name: "source", source: &progressiveFakeSource{err: sourceErr}, opener: &progressiveFakeOpener{}, want: sourceErr},
		{name: "opener", source: &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}, opener: &progressiveFakeOpener{err: openerErr}, want: openerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: tc.source, TranscodeDir: "/cache", Opener: tc.opener})
			_, err := service.OpenProgressive(t.Context(), progressiveCopyRequest())
			if !errors.Is(err, tc.want) {
				t.Fatalf("OpenProgressive() error = %v, want original %v", err, tc.want)
			}
			if tc.name == "source" && tc.opener.calls != 0 {
				t.Errorf("opener calls = %d, want no process after source error", tc.opener.calls)
			}
		})
	}
}

func TestProgressiveServiceClassifiesNilProcessDependenciesAsBackendErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opener playback.ProgressiveProcessOpener
	}{
		{name: "nil opener"},
		{name: "nil opener body", opener: &progressiveFakeOpener{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
				Sources:      &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")},
				TranscodeDir: "/cache",
				Opener:       tc.opener,
			})

			_, err := service.OpenProgressive(t.Context(), progressiveCopyRequest())
			if !errors.Is(err, playback.ErrProgressiveBackend) {
				t.Fatalf("OpenProgressive() error = %v, want ErrProgressiveBackend", err)
			}
			if errors.Is(err, playback.ErrProgressiveInvalid) {
				t.Fatalf("OpenProgressive() error = %v, must not classify backend failure as ErrProgressiveInvalid", err)
			}
		})
	}
}

type progressiveFakeSource struct {
	source         playback.MediaSourceInfo
	err            error
	calls          int
	ctx            context.Context
	userID, itemID dto.UUID
	mediaSourceID  string
}

func (source *progressiveFakeSource) StaticSource(ctx context.Context, userID, itemID dto.UUID, mediaSourceID string) (playback.MediaSourceInfo, error) {
	source.calls++
	source.ctx = ctx
	source.userID, source.itemID, source.mediaSourceID = userID, itemID, mediaSourceID
	if source.err != nil {
		return playback.MediaSourceInfo{}, source.err
	}
	return source.source, nil
}

type progressiveFakeOpener struct {
	body    io.ReadCloser
	err     error
	calls   int
	ctx     context.Context
	request playback.ProgressiveOpenRequest
}

func (opener *progressiveFakeOpener) Open(ctx context.Context, request playback.ProgressiveOpenRequest) (io.ReadCloser, error) {
	opener.calls++
	opener.ctx = ctx
	opener.request = request
	if opener.err != nil {
		return nil, opener.err
	}
	return opener.body, nil
}

func progressiveCopyOpen(t *testing.T, configure func(*playback.ProgressiveRequest)) (int, error) {
	t.Helper()
	source := &progressiveFakeSource{source: progressiveCopySource("/media/goldenrec-input.mp4")}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
	request := progressiveCopyRequest()
	configure(&request)
	_, err := service.OpenProgressive(t.Context(), request)
	return opener.calls, err
}

func progressiveCopyOpenWithSource(t *testing.T, sourceConfigure func(*playback.MediaSourceInfo), configure func(*playback.ProgressiveRequest)) (int, error) {
	t.Helper()
	sourceInfo := progressiveCopySource("/media/goldenrec-input.mp4")
	sourceConfigure(&sourceInfo)
	source := &progressiveFakeSource{source: sourceInfo}
	opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("remux"))}
	service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{Sources: source, TranscodeDir: "/cache", Opener: opener})
	request := progressiveCopyRequest()
	configure(&request)
	_, err := service.OpenProgressive(t.Context(), request)
	return opener.calls, err
}

func progressiveCopyRequest() playback.ProgressiveRequest {
	audioIndex := 1
	return playback.ProgressiveRequest{
		UserID:               progressiveUUID("00000000-0000-0000-0000-000000000701"),
		ItemID:               progressiveUUID("00000000-0000-0000-0000-000000000702"),
		MediaSourceID:        "00000000-0000-0000-0000-000000000703",
		Container:            "ts",
		DeviceID:             "device-7",
		PlaySessionID:        "session-9",
		UserAgent:            "DeadfinTest/1.0",
		VideoCodecs:          []string{"h264"},
		VideoCodec:           "h264",
		AudioCodec:           "ac3",
		AudioCodecs:          []string{"ac3"},
		AudioStreamIndex:     &audioIndex,
		AllowVideoStreamCopy: true,
		AllowAudioStreamCopy: true,
		EnableAutoStreamCopy: true,
	}
}

func progressiveCopySource(path string) playback.MediaSourceInfo {
	width, height, videoBitrate := 1280, 720, 2_600_000
	framerate := float32(30)
	profile, refFrames, videoBitDepth := "High", 4, 8
	level := float64(41)
	audioBitrate, channels, sampleRate, audioBitDepth := 384_000, 6, 48_000, 16
	size := int64(24_680)
	modifiedAt := time.Date(2026, time.July, 28, 9, 30, 0, 0, time.UTC)
	inode := int64(7_001)
	changedAt := time.Date(2026, time.July, 28, 9, 30, 1, 0, time.UTC)
	return playback.MediaSourceInfo{
		Protocol:       playback.MediaProtocolFile,
		Path:           path,
		Container:      "mov,mp4,m4a,3gp,3g2,mj2",
		SourceRevision: "test-source-revision:" + path,
		FileFacts: playback.SourceFileFacts{
			Size:       &size,
			ModifiedAt: &modifiedAt,
			Inode:      &inode,
			ChangedAt:  &changedAt,
		},
		MediaStreams: []playback.MediaStream{
			{Type: playback.StreamTypeVideo, Index: 0, Codec: "h264", Profile: &profile, Level: &level, RefFrames: &refFrames, BitDepth: &videoBitDepth, Width: &width, Height: &height, BitRate: &videoBitrate, AverageFrameRate: &framerate},
			{Type: playback.StreamTypeAudio, Index: 1, Codec: "ac3", BitRate: &audioBitrate, Channels: &channels, SampleRate: &sampleRate, BitDepth: &audioBitDepth, IsDefault: true},
		},
	}
}

func progressiveInterlacedCopySource() playback.MediaSourceInfo {
	source := progressiveCopySource("/media/goldenrec-input.mp4")
	source.MediaStreams[0].IsInterlaced = true
	return source
}

func progressiveAnamorphicCopySource() playback.MediaSourceInfo {
	source := progressiveCopySource("/media/goldenrec-input.mp4")
	anamorphic := true
	source.MediaStreams[0].IsAnamorphic = &anamorphic
	return source
}

func progressiveNonAVCCopySource() playback.MediaSourceInfo {
	source := progressiveCopySource("/media/goldenrec-input.mp4")
	avc := false
	source.MediaStreams[0].IsAVC = &avc
	return source
}

func progressiveUUID(value string) dto.UUID {
	id, err := dto.ParseUUID(value)
	if err != nil {
		panic(err)
	}
	return id
}

func progressiveArgvDifference(want, got []string) string {
	if len(want) != len(got) {
		return "length got " + itoa(len(got)) + ", want " + itoa(len(want)) + "\n got: " + strings.Join(got, " | ") + "\nwant: " + strings.Join(want, " | ")
	}
	for index := range want {
		if got[index] != want[index] {
			return "token " + itoa(index) + " got " + got[index] + ", want " + want[index] + "\n got: " + strings.Join(got, " | ") + "\nwant: " + strings.Join(want, " | ")
		}
	}
	return ""
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

// TestProgressiveServiceTranscodesWhenStreamCopyIsImpossible covers #857:
// PlaybackInfo offers /videos/{id}/stream.{container} as a progressive
// transcode, so a source that cannot be copied is encoded to H.264 and AAC
// (VideosController.GetVideoStream).
func TestProgressiveServiceTranscodesWhenStreamCopyIsImpossible(t *testing.T) {
	caps := encoding.Capabilities{H264Encoder: "libx264", AACEncoders: []string{"aac"}}
	for _, tc := range []struct {
		name        string
		container   string
		extension   string
		contentType string
	}{
		{name: "matroska", container: "mkv", extension: ".mkv", contentType: "video/x-matroska"},
		{name: "mpeg-ts", container: "ts", extension: ".ts", contentType: "video/mp2t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/goldenrec-input.mkv")
			sourceInfo.MediaStreams[0].Codec = "vc1"
			source := &progressiveFakeSource{source: sourceInfo}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("transcode"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
				Sources: source, TranscodeDir: "/cache", Opener: opener,
				Capabilities: func() encoding.Capabilities { return caps },
			})
			request := progressiveCopyRequest()
			request.Container = tc.container
			request.AllowVideoStreamCopy = false
			request.AudioCodecs, request.AudioCodec = []string{"aac"}, "aac"
			videoBitrate := 1_500_000
			request.VideoBitrate = &videoBitrate

			response, err := service.OpenProgressive(t.Context(), request)
			if err != nil {
				t.Fatalf("OpenProgressive() error = %v", err)
			}
			if response.ContentType != tc.contentType || !strings.HasSuffix(response.OutputPath, tc.extension) {
				t.Fatalf("response = %q %q, want %s output with %s", response.ContentType, response.OutputPath, tc.contentType, tc.extension)
			}
			args := strings.Join(opener.request.Args, " ")
			for _, want := range []string{"-codec:v:0 libx264", "-maxrate 1500000", "-codec:a:0 aac", "-ac 6"} {
				if !strings.Contains(args, want) {
					t.Errorf("argv %q lacks %q", args, want)
				}
			}
			if last := opener.request.Args[len(opener.request.Args)-1]; last != response.OutputPath {
				t.Errorf("argv output = %q, want %q", last, response.OutputPath)
			}
		})
	}
}

func TestProgressiveServiceRefusesTranscodesItCannotEncode(t *testing.T) {
	subtitleEncode := playback.SubtitleEncode
	selected := 0
	caps := encoding.Capabilities{H264Encoder: "libx264", AACEncoders: []string{"aac"}}
	for _, tc := range []struct {
		name    string
		caps    encoding.Capabilities
		request func(*playback.ProgressiveRequest)
	}{
		{name: "no libx264", request: func(*playback.ProgressiveRequest) {}},
		{name: "HEVC only", caps: caps, request: func(r *playback.ProgressiveRequest) { r.VideoCodecs, r.VideoCodec = []string{"hevc"}, "hevc" }},
		{name: "subtitle burn-in", caps: caps, request: func(r *playback.ProgressiveRequest) {
			r.SubtitleDeliveryMethod, r.SubtitleStreamIndex = &subtitleEncode, &selected
		}},
		{name: "audio codec without AAC", caps: caps, request: func(r *playback.ProgressiveRequest) {
			r.AllowAudioStreamCopy = false
			r.AudioCodecs, r.AudioCodec = []string{"mp3"}, "mp3"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sourceInfo := progressiveCopySource("/media/goldenrec-input.mkv")
			sourceInfo.MediaStreams[0].Codec = "vc1"
			source := &progressiveFakeSource{source: sourceInfo}
			opener := &progressiveFakeOpener{body: io.NopCloser(strings.NewReader("must not open"))}
			service := playback.NewProgressiveService(playback.ProgressiveServiceConfig{
				Sources: source, TranscodeDir: "/cache", Opener: opener,
				Capabilities: func() encoding.Capabilities { return tc.caps },
			})
			request := progressiveCopyRequest()
			request.Container = "mkv"
			tc.request(&request)
			if _, err := service.OpenProgressive(t.Context(), request); !errors.Is(err, playback.ErrProgressiveUnsupported) || opener.calls != 0 {
				t.Fatalf("OpenProgressive() error = %v, opens = %d; want ErrProgressiveUnsupported and no open", err, opener.calls)
			}
		})
	}
}
