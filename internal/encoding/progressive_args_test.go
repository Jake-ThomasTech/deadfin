// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type progressiveGoldenCorpus struct {
	Cases                 []progressiveGoldenCase `json:"cases"`
	ProgressiveRemuxCases []progressiveGoldenCase `json:"progressiveRemuxCases"`
	ProgressiveEdgeCases  []progressiveGoldenCase `json:"progressiveEdgeCases"`
}

type progressiveGoldenCase struct {
	ID                   string                `json:"id"`
	Fixture              string                `json:"fixture"`
	InputCodec           string                `json:"inputCodec"`
	Subtitle             string                `json:"subtitle"`
	SubtitleCodec        string                `json:"subtitleCodec"`
	SubtitlePath         string                `json:"subtitlePath"`
	SubtitleInputIndex   int                   `json:"subtitleInputIndex"`
	SubtitleStreamIndex  int                   `json:"subtitleStreamIndex"`
	ToneMap              string                `json:"tonemap"`
	Audio                string                `json:"audio"`
	AudioCodec           string                `json:"audioCodec"`
	AudioStreamIndex     int                   `json:"audioStreamIndex"`
	AudioChannels        int                   `json:"audioChannels"`
	DownmixAudioBoost    *float64              `json:"downmixAudioBoost"`
	NalLengthSize        string                `json:"nalLengthSize"`
	Capabilities         progressiveGoldenCaps `json:"capabilities"`
	Container            string                `json:"container"`
	Seek                 string                `json:"seek"`
	SeekOffsetTicks      int64                 `json:"seekOffsetTicks"`
	SelectedVideoCodec   string                `json:"selectedVideoCodec"`
	SelectedAudioCodec   string                `json:"selectedAudioCodec"`
	SelectedVideoEncoder *string               `json:"selectedVideoEncoder"`
	SelectedAudioEncoder *string               `json:"selectedAudioEncoder"`
	Argv                 progressiveGoldenArgv `json:"argv"`
}

type progressiveGoldenCaps struct {
	H264Encoder         string   `json:"h264Encoder"`
	EncoderMajorVersion int      `json:"encoderMajorVersion"`
	AACEncoders         []string `json:"aacEncoders"`
	SupportsTonemapX    bool     `json:"supportsTonemapx"`
}

type progressiveGoldenArgv struct {
	Progressive string `json:"progressive"`
	HLS         string `json:"hls"`
}

func TestBuildProgressiveArgsMatchesGoldenCorpus(t *testing.T) {
	corpus := loadProgressiveGoldenCorpus(t)
	cases := make([]progressiveGoldenCase, 0, len(corpus.Cases)+len(corpus.ProgressiveRemuxCases)+len(corpus.ProgressiveEdgeCases))
	cases = append(cases, corpus.Cases...)
	cases = append(cases, corpus.ProgressiveRemuxCases...)
	cases = append(cases, corpus.ProgressiveEdgeCases...)
	if got, want := len(corpus.Cases), 432; got != want {
		t.Fatalf("software transcode case count = %d, want %d", got, want)
	}
	if got, want := len(corpus.ProgressiveRemuxCases), 4; got != want {
		t.Fatalf("progressive remux case count = %d, want %d", got, want)
	}
	if got, want := len(corpus.ProgressiveEdgeCases), 1; got != want {
		t.Fatalf("progressive edge case count = %d, want %d", got, want)
	}

	for _, golden := range cases {
		t.Run(golden.ID, func(t *testing.T) {
			assertProgressiveGoldenSourceFacts(t, golden)
			want := splitFFmpegArgv(t, golden.Argv.Progressive)
			got, err := BuildProgressiveArgs(progressiveJobFromGolden(t, golden), progressiveCapabilitiesFromGolden(golden.Capabilities))
			if err != nil {
				t.Fatalf("BuildProgressiveArgs: %v", err)
			}
			if difference := argvDifference(want, got); difference != "" {
				t.Fatalf("progressive argv mismatch for %s:\n%s", golden.ID, difference)
			}
		})
	}
}

func TestBuildProgressiveArgsRejectsInvalidJob(t *testing.T) {
	valid := progressiveJobFromGolden(t, progressiveGoldenCase{
		Fixture:         "mp4-h264-ac3-aac-srt-2600k",
		InputCodec:      "h264",
		Subtitle:        "none",
		ToneMap:         "sdr",
		Audio:           "copy",
		Container:       "ts",
		Seek:            "start",
		SeekOffsetTicks: 0,
	})

	for _, test := range []struct {
		name string
		job  EncodingJob
	}{
		{name: "empty input path", job: func() EncodingJob { job := valid; job.InputPath = ""; return job }()},
		{name: "unsafe output path", job: func() EncodingJob { job := valid; job.OutputPath = "/cache/out.ts\x00-smuggled"; return job }()},
		{name: "unsupported output codec", job: func() EncodingJob { job := valid; job.OutputVideoCodec = "vp9"; return job }()},
		{name: "unsupported output container", job: func() EncodingJob { job := valid; job.OutputContainer = "mkv"; return job }()},
		{name: "negative seek", job: func() EncodingJob { job := valid; job.StartOffsetTicks = -1; return job }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildProgressiveArgs(test.job, progressiveTestCapabilities()); err == nil {
				t.Fatal("BuildProgressiveArgs accepted invalid job")
			}
		})
	}
}

func TestBuildProgressiveArgsSelectsAACEncoderByCapabilityOrder(t *testing.T) {
	for _, test := range []struct {
		name     string
		encoders []string
		want     string
	}{
		{name: "audiotoolbox preferred", encoders: []string{"aac_at", "libfdk_aac", "aac"}, want: "aac_at"},
		{name: "fdk fallback", encoders: []string{"libfdk_aac", "aac"}, want: "libfdk_aac"},
		{name: "native fallback", encoders: []string{"aac"}, want: "aac"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.OutputAudioCodec = "aac"
			args := mustBuildProgressiveArgs(t, job, Capabilities{H264Encoder: "libx264", AACEncoders: test.encoders, SupportsTonemapX: true})
			if got := argumentValue(t, args, "-codec:a:0"); got != test.want {
				t.Fatalf("AAC encoder = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildProgressiveArgsRejectsUnsupportedHDRToneMapping(t *testing.T) {
	for _, test := range []struct {
		name             string
		bitDepth         int
		supportsTonemapX bool
		wantError        bool
	}{
		{name: "eight bit HDR", bitDepth: 8, supportsTonemapX: true, wantError: true},
		{name: "tonemapx unavailable", bitDepth: 10, supportsTonemapX: false, wantError: true},
		{name: "supported HDR", bitDepth: 10, supportsTonemapX: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.VideoStream.ColorTransfer = "smpte2084"
			job.VideoStream.BitDepth = test.bitDepth
			args, err := BuildProgressiveArgs(job, Capabilities{H264Encoder: "libx264", AACEncoders: []string{"aac"}, SupportsTonemapX: test.supportsTonemapX})
			if test.wantError {
				if err == nil {
					t.Fatal("BuildProgressiveArgs accepted unsupported HDR tone mapping")
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildProgressiveArgs: %v", err)
			}
			if filter := argumentValue(t, args, "-vf"); !strings.Contains(filter, "tonemapx=") {
				t.Fatalf("HDR filter %q does not include tonemapx", filter)
			}
		})
	}
}

func TestBuildProgressiveArgsUsesNalLengthSizeForAnnexB(t *testing.T) {
	for _, test := range []struct {
		name          string
		nalLengthSize string
		wantAnnexB    bool
	}{
		{name: "annex B input", nalLengthSize: "0"},
		{name: "length prefixed input", nalLengthSize: "4", wantAnnexB: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.InputContainer = "mkv"
			job.OutputVideoCodec = "copy"
			job.OutputAudioCodec = "copy"
			job.VideoStream.NalLengthSize = test.nalLengthSize
			args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
			gotAnnexB := hasArgumentValue(args, "-bsf:v", "h264_mp4toannexb")
			if gotAnnexB != test.wantAnnexB {
				t.Fatalf("NalLengthSize %q emitted Annex-B filter = %t, want %t; argv = %q", test.nalLengthSize, gotAnnexB, test.wantAnnexB, args)
			}
		})
	}
}

func TestBuildProgressiveArgsUsesExplicitPGSInputAndStreamIndexes(t *testing.T) {
	job := progressiveUnitJob()
	job.SubtitleStream = &SubtitleStream{InputIndex: 2, Index: 7, Codec: "pgssub", BurnIn: true}
	args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
	filter := argumentValue(t, args, "-filter_complex")
	if !strings.Contains(filter, "[2:7]") {
		t.Fatalf("PGS filter %q does not identify the explicit input and stream", filter)
	}
}

func TestBuildProgressiveArgsUsesTextSubtitlePathAndPinnedFractionalPTS(t *testing.T) {
	for _, test := range []struct {
		name        string
		ticks       int64
		wantSeconds string
	}{
		{name: "one point five seconds rounds to even two", ticks: 15_000_000, wantSeconds: "2"},
		{name: "two point five seconds rounds to even two", ticks: 25_000_000, wantSeconds: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.OutputAudioCodec = "aac"
			job.StartOffsetTicks = test.ticks
			job.SubtitleStream = &SubtitleStream{InputIndex: 0, Index: 3, Codec: "srt", Path: `/cache/quoted "subtitle": O'Brien.srt`, BurnIn: true}
			args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
			filter := argumentValue(t, args, "-vf")
			for _, want := range []string{"subtitles=f='", `\:`, `\"`, "setpts=PTS -" + test.wantSeconds + "/TB"} {
				if !strings.Contains(filter, want) {
					t.Fatalf("text subtitle filter %q does not contain %q", filter, want)
				}
			}
			audioFilter := argumentValue(t, args, "-af")
			if want := "asetpts=PTS-" + test.wantSeconds + "/TB"; !strings.Contains(audioFilter, want) {
				t.Fatalf("audio filter %q does not contain %q", audioFilter, want)
			}
		})
	}
}

func TestBuildProgressiveArgsAppliesNumericDownmixBoostForMultichannelStereoOutput(t *testing.T) {
	for _, test := range []struct {
		name       string
		codec      string
		channels   int
		boost      *float64
		wantVolume string
	}{
		{name: "omitted boost is unity", codec: "ac3", channels: 6},
		{name: "explicit unity has no volume filter", codec: "ac3", channels: 6, boost: float64Pointer(1)},
		{name: "fractional boost", codec: "ac3", channels: 6, boost: float64Pointer(1.5), wantVolume: "volume=1.5"},
		{name: "double boost", codec: "ac3", channels: 6, boost: float64Pointer(2), wantVolume: "volume=2"},
		{name: "zero boost", codec: "ac3", channels: 6, boost: float64Pointer(0), wantVolume: "volume=0"},
		{name: "stereo AC3 ignores numeric boost", codec: "ac3", channels: 2, boost: float64Pointer(2)},
		{name: "multichannel AAC uses numeric boost", codec: "aac", channels: 6, boost: float64Pointer(1.5), wantVolume: "volume=1.5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.AudioStream.Codec = test.codec
			job.AudioStream.Channels = test.channels
			job.OutputAudioCodec = "aac"
			job.OutputAudioChannels = 2
			job.DownmixAudioBoost = test.boost
			args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
			audioFilter := optionalArgumentValue(args, "-af")
			if test.wantVolume == "" {
				if strings.Contains(audioFilter, "volume=") {
					t.Fatalf("downmix boost %#v emitted unexpected filter %q", test.boost, audioFilter)
				}
				return
			}
			if !strings.Contains(audioFilter, test.wantVolume) {
				t.Fatalf("downmix boost %#v emitted filter %q, want %q", test.boost, audioFilter, test.wantVolume)
			}
		})
	}
}

func float64Pointer(value float64) *float64 { return &value }

func TestBuildProgressiveArgsRejectsUnsupportedSoftwareEncodingPlans(t *testing.T) {
	for _, test := range []struct {
		name string
		job  func(EncodingJob) EncodingJob
		caps Capabilities
	}{
		{name: "non libx264 encoder", job: func(job EncodingJob) EncodingJob { return job }, caps: Capabilities{H264Encoder: "h264_videotoolbox", AACEncoders: []string{"aac"}, SupportsTonemapX: true}},
		{name: "copy and burn in", job: func(job EncodingJob) EncodingJob {
			job.OutputVideoCodec = "copy"
			job.SubtitleStream = &SubtitleStream{InputIndex: 0, Index: 3, Codec: "srt", Path: "/cache/subtitles.srt", BurnIn: true}
			return job
		}, caps: progressiveTestCapabilities()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildProgressiveArgs(test.job(progressiveUnitJob()), test.caps); err == nil {
				t.Fatal("BuildProgressiveArgs accepted unsupported software encoding plan")
			}
		})
	}
}

func TestBuildProgressiveArgsRejectsInvalidQualityBounds(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, test := range []struct {
		name string
		job  func(EncodingJob) EncodingJob
	}{
		{name: "unknown preset", job: func(job EncodingJob) EncodingJob { job.VideoQuality.Preset = "unbounded"; return job }},
		{name: "CRF above H264 range", job: func(job EncodingJob) EncodingJob { job.VideoQuality.CRF = 52; return job }},
		{name: "video bitrate buffer overflow", job: func(job EncodingJob) EncodingJob { job.OutputVideoBitrate = maxInt; return job }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildProgressiveArgs(test.job(progressiveUnitJob()), progressiveTestCapabilities()); err == nil {
				t.Fatal("BuildProgressiveArgs accepted invalid H.264 quality settings")
			}
		})
	}
}

func TestBuildProgressiveArgsPermitsNoAudio(t *testing.T) {
	job := progressiveUnitJob()
	job.AudioStream = AudioStream{}
	job.OutputAudioCodec = ""
	job.OutputAudioBitrate = 0
	args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
	if hasArgument(args, "-codec:a:0") {
		t.Fatalf("no-audio job emitted an audio codec: %q", args)
	}
}

func progressiveUnitJob() EncodingJob {
	return EncodingJob{
		InputPath:          "/media/input.mkv",
		InputContainer:     "mkv",
		OutputPath:         "/cache/progressive.ts",
		OutputContainer:    "ts",
		VideoStream:        VideoStream{Index: 0, Codec: "h264", BitDepth: 8, NalLengthSize: "0"},
		AudioStream:        AudioStream{Index: 1, Codec: "ac3", Channels: 6},
		OutputVideoCodec:   "h264",
		OutputAudioCodec:   "aac",
		OutputWidth:        1280,
		OutputHeight:       720,
		OutputVideoBitrate: 2_500_000,
		OutputAudioBitrate: 192_000,
		VideoQuality: VideoQuality{
			Preset:                "veryfast",
			CRF:                   23,
			ForcedKeyFrameSeconds: 5,
			X264Options:           "subme=0:me_range=16:rc_lookahead=10:me=hex:open_gop=0",
		},
	}
}

func mustBuildProgressiveArgs(t *testing.T, job EncodingJob, caps Capabilities) []string {
	t.Helper()
	args, err := BuildProgressiveArgs(job, caps)
	if err != nil {
		t.Fatalf("BuildProgressiveArgs: %v", err)
	}
	return args
}

func argumentValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	t.Fatalf("argv %q lacks %s", args, flag)
	return ""
}

func optionalArgumentValue(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

func hasArgumentValue(args []string, flag, want string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && args[index+1] == want {
			return true
		}
	}
	return false
}

func hasArgument(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestSplitFFmpegArgvDecodesRecordedShellSyntax(t *testing.T) {
	command := ` -i file:"/media/input with spaces.mkv" -filter_complex "scale=max(1280\,iw):720" -metadata 'title=quoted value' bare\ value ""`
	want := []string{
		"-i",
		"file:/media/input with spaces.mkv",
		"-filter_complex",
		"scale=max(1280\\,iw):720",
		"-metadata",
		"title=quoted value",
		"bare value",
		"",
	}
	if difference := argvDifference(want, splitFFmpegArgv(t, command)); difference != "" {
		t.Fatalf("splitFFmpegArgv mismatch:\n%s", difference)
	}
}

func loadProgressiveGoldenCorpus(t *testing.T) progressiveGoldenCorpus {
	t.Helper()
	contents, err := os.ReadFile("testdata/golden/encoding.json")
	if err != nil {
		t.Fatalf("read encoding corpus: %v", err)
	}
	var corpus progressiveGoldenCorpus
	if err := json.Unmarshal(contents, &corpus); err != nil {
		t.Fatalf("decode encoding corpus: %v", err)
	}
	return corpus
}

func progressiveJobFromGolden(t *testing.T, golden progressiveGoldenCase) EncodingJob {
	t.Helper()
	inputContainer, inputExtension, video := progressiveFixtureFacts(t, golden.Fixture)
	if golden.InputCodec != "" {
		video.Codec = golden.InputCodec
	}
	applyProgressiveRange(t, &video, golden.ToneMap)
	video.NalLengthSize = golden.NalLengthSize

	job := EncodingJob{
		InputPath:          "/media/goldenrec-input." + inputExtension,
		InputContainer:     inputContainer,
		OutputPath:         "/cache/goldenrec-progressive." + progressiveOutputExtension(t, golden.Container),
		OutputContainer:    progressiveOutputContainer(t, golden.Container),
		VideoStream:        video,
		AudioStream:        AudioStream{Index: golden.AudioStreamIndex, Codec: golden.AudioCodec, Channels: golden.AudioChannels},
		OutputVideoCodec:   "h264",
		OutputAudioCodec:   progressiveOutputAudioCodec(t, golden.Audio, golden.SelectedAudioCodec),
		OutputWidth:        1280,
		OutputHeight:       720,
		OutputVideoBitrate: 2_500_000,
		StartOffsetTicks:   golden.SeekOffsetTicks,
		CopyTimestamps:     false,
		DownmixAudioBoost:  golden.DownmixAudioBoost,
		VideoQuality: VideoQuality{
			Preset:                "veryfast",
			CRF:                   23,
			ForcedKeyFrameSeconds: 5,
			X264Options:           "subme=0:me_range=16:rc_lookahead=10:me=hex:open_gop=0",
		},
	}

	if golden.Audio != "copy" && golden.SelectedAudioCodec == "" {
		job.OutputAudioBitrate = 192_000
	}
	if golden.Audio == "downmix" {
		job.OutputAudioChannels = 2
	}
	if golden.Subtitle != "" && golden.Subtitle != "none" {
		job.SubtitleStream = &SubtitleStream{
			InputIndex: golden.SubtitleInputIndex,
			Index:      golden.SubtitleStreamIndex,
			Codec:      golden.SubtitleCodec,
			Path:       golden.SubtitlePath,
			BurnIn:     true,
			IsExternal: golden.SubtitlePath != "",
		}
	}
	if golden.SelectedVideoCodec == "copy" {
		job.OutputVideoCodec = "copy"
	}
	if golden.SelectedAudioCodec == "copy" {
		job.OutputAudioCodec = "copy"
	}
	return job
}

func assertProgressiveGoldenSourceFacts(t *testing.T, golden progressiveGoldenCase) {
	t.Helper()
	if strings.EqualFold(golden.InputCodec, "h264") && golden.NalLengthSize == "" {
		t.Fatal("H.264 corpus row omits source NalLengthSize")
	}
	if golden.AudioCodec == "" || golden.AudioChannels <= 0 {
		t.Fatalf("corpus row has incomplete source audio facts: codec=%q channels=%d", golden.AudioCodec, golden.AudioChannels)
	}
	if golden.DownmixAudioBoost == nil || *golden.DownmixAudioBoost != 2 {
		t.Fatalf("corpus row has non-canonical numeric downmix boost: %#v", golden.DownmixAudioBoost)
	}
	if golden.Capabilities.H264Encoder == "" || golden.Capabilities.EncoderMajorVersion <= 0 || len(golden.Capabilities.AACEncoders) == 0 {
		t.Fatalf("corpus row omits encoder capability facts: %#v", golden.Capabilities)
	}
	if golden.Subtitle == "text" && (golden.SubtitlePath == "" || golden.SubtitleCodec == "") {
		t.Fatalf("text subtitle row omits resolved source facts: %#v", golden)
	}
	if golden.Subtitle == "pgs" && golden.SubtitleCodec == "" {
		t.Fatalf("PGS subtitle row omits source codec/index facts: %#v", golden)
	}
	if golden.SelectedVideoCodec == "copy" && golden.SelectedVideoEncoder != nil {
		t.Fatalf("stream-copy row has selected video encoder %q", *golden.SelectedVideoEncoder)
	}
	if golden.SelectedAudioCodec == "copy" && golden.SelectedAudioEncoder != nil {
		t.Fatalf("stream-copy row has selected audio encoder %q", *golden.SelectedAudioEncoder)
	}
}

func progressiveCapabilitiesFromGolden(golden progressiveGoldenCaps) Capabilities {
	return Capabilities{
		H264Encoder:         golden.H264Encoder,
		EncoderMajorVersion: golden.EncoderMajorVersion,
		AACEncoders:         golden.AACEncoders,
		SupportsTonemapX:    golden.SupportsTonemapX,
	}
}

func progressiveFixtureFacts(t *testing.T, fixture string) (string, string, VideoStream) {
	t.Helper()
	switch fixture {
	case "mp4-h264-ac3-aac-srt-2600k":
		return "mov,mp4,m4a,3gp,3g2,mj2", "mp4", VideoStream{Index: 0, Codec: "h264", CodecTag: "avc1", BitDepth: 8}
	case "mp4-hevc-ac3-aac-srt-15200k":
		return "mov,mp4,m4a,3gp,3g2,mj2", "mp4", VideoStream{Index: 0, Codec: "hevc", CodecTag: "hev1", ColorTransfer: "smpte2084", BitDepth: 8}
	case "mkv-av1-aac-srt-2600k":
		return "mkv", "mkv", VideoStream{Index: 0, Codec: "av1", ColorTransfer: "bt709", BitDepth: 8}
	default:
		t.Fatalf("unknown corpus fixture %q", fixture)
		return "", "", VideoStream{}
	}
}

func progressiveOutputAudioCodec(t *testing.T, audio, selectedCodec string) string {
	t.Helper()
	if selectedCodec != "" {
		return selectedCodec
	}
	switch audio {
	case "copy":
		return "copy"
	case "aac", "downmix":
		return "aac"
	default:
		t.Fatalf("unknown audio variant %q", audio)
		return ""
	}
}

func progressiveOutputContainer(t *testing.T, container string) string {
	t.Helper()
	switch container {
	case "ts":
		return "ts"
	case "fmp4":
		return "mp4"
	default:
		t.Fatalf("unknown output container %q", container)
		return ""
	}
}

func progressiveOutputExtension(t *testing.T, container string) string {
	t.Helper()
	switch container {
	case "ts":
		return "ts"
	case "fmp4":
		return "mp4"
	default:
		t.Fatalf("unknown output container %q", container)
		return ""
	}
}

func applyProgressiveRange(t *testing.T, video *VideoStream, toneMap string) {
	t.Helper()
	switch toneMap {
	case "", "sdr":
		video.ColorTransfer = ""
		video.CodecTag = ""
		video.BitDepth = 8
		video.DolbyVisionProfile = 0
		video.RPUFlag = 0
		video.BLPresentFlag = 0
		video.DolbyVisionBLCompatibilityID = 0
	case "hdr10":
		video.ColorTransfer = "smpte2084"
		video.BitDepth = 10
	case "hlg":
		video.ColorTransfer = "arib-std-b67"
		video.BitDepth = 10
	case "dv":
		video.CodecTag = "dvhe"
		video.BitDepth = 10
		video.DolbyVisionProfile = 5
		video.RPUFlag = 1
		video.BLPresentFlag = 1
		video.DolbyVisionBLCompatibilityID = 0
	default:
		t.Fatalf("unknown tone-map range %q", toneMap)
	}
}

func progressiveTestCapabilities() Capabilities {
	return Capabilities{
		H264Encoder:                "libx264",
		AACEncoders:                []string{"aac_at", "libfdk_aac", "aac"},
		SupportsTonemapX:           true,
		EncoderMajorVersion:        7,
		SupportsSubtitleExtraction: false,
	}
}

// splitFFmpegArgv decodes the recorder's command-line representation into the
// logical argument vector passed to ffmpeg. It supports unquoted, single-quoted,
// and double-quoted text; backslash escaping outside quotes; and POSIX-compatible
// backslash handling inside double quotes.
func splitFFmpegArgv(t *testing.T, command string) []string {
	t.Helper()
	var args []string
	var current strings.Builder
	quoted := byte(0)
	started := false

	flush := func() {
		if started {
			args = append(args, current.String())
			current.Reset()
			started = false
		}
	}

	for index := 0; index < len(command); index++ {
		character := command[index]
		switch quoted {
		case 0:
			switch character {
			case ' ', '\t', '\n', '\r':
				flush()
			case '\'', '"':
				quoted = character
				started = true
			case '\\':
				if index+1 == len(command) {
					t.Fatalf("trailing escape in recorded argv %q", command)
				}
				index++
				current.WriteByte(command[index])
				started = true
			default:
				current.WriteByte(character)
				started = true
			}
		case '\'':
			if character == '\'' {
				quoted = 0
			} else {
				current.WriteByte(character)
			}
		case '"':
			if character == '"' {
				quoted = 0
				continue
			}
			if character == '\\' && index+1 < len(command) {
				next := command[index+1]
				switch next {
				case '"', '\\', '$', '`':
					current.WriteByte(next)
					index++
					continue
				case '\n':
					index++
					continue
				}
			}
			current.WriteByte(character)
		}
	}
	if quoted != 0 {
		t.Fatalf("unterminated quote in recorded argv %q", command)
	}
	flush()
	return args
}

func argvDifference(want, got []string) string {
	limit := len(want)
	if len(got) < limit {
		limit = len(got)
	}
	for index := range limit {
		if want[index] != got[index] {
			return fmt.Sprintf("token %d:\n  want %q\n   got %q\nwant argv: %q\n got argv: %q", index, want[index], got[index], want, got)
		}
	}
	if len(want) != len(got) {
		return fmt.Sprintf("token count:\n  want %d\n   got %d\nwant argv: %q\n got argv: %q", len(want), len(got), want, got)
	}
	return ""
}

// A live job has no input path; a text subtitle burn-in must read the broker
// URL instead of an empty filename.
func TestLiveTextSubtitleBurnInReadsTheBrokerURL(t *testing.T) {
	job := progressiveUnitJob()
	job.InputPath = ""
	job.LiveInput = &LiveInput{URL: "http://127.0.0.1:8096/LiveTv/LiveStreamFiles/x/stream.ts"}
	job.SubtitleStream = &SubtitleStream{InputIndex: 0, Index: 2, SubtitleIndex: 0, Codec: "subrip", BurnIn: true}

	args := mustBuildProgressiveArgs(t, job, progressiveTestCapabilities())
	filter := argumentValue(t, args, "-vf")
	if !strings.Contains(filter, `subtitles=f='http\://127.0.0.1\:8096/LiveTv/LiveStreamFiles/x/stream.ts'`) {
		t.Errorf("-vf = %q, want the broker URL as the subtitle source", filter)
	}
}
