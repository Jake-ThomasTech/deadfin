// SPDX-License-Identifier: GPL-2.0-only

package encoding

import (
	"math"
	"strings"
	"testing"
)

func TestSoftwareTranscodesEmitOnlyRequestedOutputFramerate(t *testing.T) {
	for _, test := range []struct {
		name      string
		frameRate float64
		wantRate  string
	}{
		{name: "capped sixty fps source", frameRate: 30, wantRate: "30"},
		{name: "explicit twenty four fps target", frameRate: 24, wantRate: "24"},
		{name: "absent target", frameRate: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, builder := range []struct {
				name  string
				build func(EncodingJob) ([]string, error)
			}{
				{
					name: "progressive",
					build: func(job EncodingJob) ([]string, error) {
						return BuildProgressiveArgs(job, progressiveTestCapabilities())
					},
				},
				{
					name: "hls",
					build: func(job EncodingJob) ([]string, error) {
						return BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3}, progressiveTestCapabilities())
					},
				},
			} {
				t.Run(builder.name, func(t *testing.T) {
					job := progressiveUnitJob()
					if builder.name == "hls" {
						job.OutputPath = "/cache/master.m3u8"
						job.VideoQuality.ForcedKeyFrameSeconds = 3
					}
					job.OutputFrameRate = test.frameRate

					args, err := builder.build(job)
					if err != nil {
						t.Fatalf("Build %s args: %v", builder.name, err)
					}
					gotRate := optionalArgumentValue(args, "-r")
					if gotRate != test.wantRate {
						t.Fatalf("%s output rate = %q, want %q; argv = %q", builder.name, gotRate, test.wantRate, args)
					}
				})
			}
		})
	}
}

func TestZeroOutputFrameratePreservesRecordedGoldenArgv(t *testing.T) {
	corpus := loadProgressiveGoldenCorpus(t)
	if len(corpus.Cases) == 0 {
		t.Fatal("encoding corpus has no software transcode cases")
	}
	golden := corpus.Cases[0]
	job := progressiveJobFromGolden(t, golden)
	job.OutputFrameRate = 0

	progressive, err := BuildProgressiveArgs(job, progressiveCapabilitiesFromGolden(golden.Capabilities))
	if err != nil {
		t.Fatalf("BuildProgressiveArgs: %v", err)
	}
	if difference := argvDifference(splitFFmpegArgv(t, golden.Argv.Progressive), progressive); difference != "" {
		t.Fatalf("zero output framerate changed recorded progressive argv:\n%s", difference)
	}

	hlsJob := hlsJobFromGolden(t, golden)
	hlsJob.OutputFrameRate = 0
	hls, err := BuildHLSArgs(hlsJob, progressiveCapabilitiesFromGolden(golden.Capabilities))
	if err != nil {
		t.Fatalf("BuildHLSArgs: %v", err)
	}
	if difference := argvDifference(splitFFmpegArgv(t, golden.Argv.HLS), hls); difference != "" {
		t.Fatalf("zero output framerate changed recorded HLS argv:\n%s", difference)
	}
}

func TestSoftwareTranscodesBoundDimensionsBeforeArgvConstruction(t *testing.T) {
	for _, dimension := range []struct {
		name             string
		width, height    int
		wantValid        bool
		wantFilterPrefix string
	}{
		{
			name:             "maximum 8K pixel budget",
			width:            8_192,
			height:           4_096,
			wantValid:        true,
			wantFilterPrefix: "scale=trunc(8192/2)*2:trunc(4096/2)*2",
		},
		{
			name:             "8K UHD",
			width:            7_680,
			height:           4_320,
			wantValid:        true,
			wantFilterPrefix: "scale=trunc(7680/2)*2:trunc(4320/2)*2",
		},
		{name: "first axis over limit", width: 8_193, height: 1},
		{name: "first pixel-budget row over limit", width: 8_192, height: 4_097},
		{name: "int32 maximum width", width: math.MaxInt32, height: 1},
		{name: "int32 maximum height", width: 1, height: math.MaxInt32},
		{name: "int32 maximum product", width: math.MaxInt32, height: math.MaxInt32},
		{name: "int32 minimum width", width: math.MinInt32, height: 1},
		{name: "negative height", width: 1, height: -1},
		{
			// A live source that probed without a size transcodes at the
			// source size.
			name:             "unknown size keeps the source size",
			wantValid:        true,
			wantFilterPrefix: "scale=0:0",
		},
		{
			name:             "unknown height keeps the aspect ratio",
			width:            1_280,
			wantValid:        true,
			wantFilterPrefix: "scale=trunc(1280/2)*2:-2",
		},
		{
			name:             "unknown width keeps the aspect ratio",
			height:           720,
			wantValid:        true,
			wantFilterPrefix: "scale=-2:trunc(720/2)*2",
		},
	} {
		t.Run(dimension.name, func(t *testing.T) {
			for _, builder := range []struct {
				name  string
				build func(EncodingJob) ([]string, error)
			}{
				{
					name: "progressive",
					build: func(job EncodingJob) ([]string, error) {
						return BuildProgressiveArgs(job, progressiveTestCapabilities())
					},
				},
				{
					name: "HLS",
					build: func(job EncodingJob) ([]string, error) {
						return BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3}, progressiveTestCapabilities())
					},
				},
			} {
				t.Run(builder.name, func(t *testing.T) {
					job := progressiveUnitJob()
					if builder.name == "HLS" {
						job.OutputPath = "/cache/master.m3u8"
						job.VideoQuality.ForcedKeyFrameSeconds = 3
					}
					job.OutputWidth, job.OutputHeight = dimension.width, dimension.height

					args, err := builder.build(job)
					if dimension.wantValid {
						if err != nil {
							t.Fatalf("Build%sArgs(%d x %d): %v", builder.name, dimension.width, dimension.height, err)
						}
						if !strings.Contains(argumentValue(t, args, "-vf"), dimension.wantFilterPrefix) {
							t.Fatalf("Build%sArgs(%d x %d) filter = %q, want %q; argv = %q", builder.name, dimension.width, dimension.height, argumentValue(t, args, "-vf"), dimension.wantFilterPrefix, args)
						}
						return
					}

					if err == nil {
						t.Fatalf("Build%sArgs(%d x %d) error = nil, want dimension rejection; argv = %q", builder.name, dimension.width, dimension.height, args)
					}
					if args != nil {
						t.Fatalf("Build%sArgs(%d x %d) argv = %q, want no argv after dimension rejection", builder.name, dimension.width, dimension.height, args)
					}
				})
			}
		})
	}
}

func TestSoftwareTranscodesRejectUnsupportedRequestedVideoCodec(t *testing.T) {
	for _, test := range []struct {
		name      string
		codec     string
		wantError bool
	}{
		{name: "H264 is supported", codec: "h264"},
		{name: "AV1 is not implemented", codec: "av1", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, builder := range []struct {
				name  string
				build func(EncodingJob) ([]string, error)
			}{
				{
					name: "progressive",
					build: func(job EncodingJob) ([]string, error) {
						return BuildProgressiveArgs(job, progressiveTestCapabilities())
					},
				},
				{
					name: "hls",
					build: func(job EncodingJob) ([]string, error) {
						return BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3}, progressiveTestCapabilities())
					},
				},
			} {
				t.Run(builder.name, func(t *testing.T) {
					job := progressiveUnitJob()
					if builder.name == "hls" {
						job.OutputPath = "/cache/master.m3u8"
						job.VideoQuality.ForcedKeyFrameSeconds = 3
					}
					job.OutputVideoCodec = test.codec
					args, err := builder.build(job)
					if (err != nil) != test.wantError {
						t.Fatalf("Build %s args with video codec %q error = %v, want error = %t", builder.name, test.codec, err, test.wantError)
					}
					if !test.wantError && argumentValue(t, args, "-codec:v:0") != "libx264" {
						t.Fatalf("Build %s H264 codec token = %q, want libx264; argv = %q", builder.name, argumentValue(t, args, "-codec:v:0"), args)
					}
				})
			}
		})
	}
}

func TestBuildHLSArgsPermitsHEVCCopyForFMP4Master(t *testing.T) {
	job := progressiveUnitJob()
	job.OutputPath = "/cache/hevc-master.m3u8"
	job.OutputContainer = "fmp4"
	job.OutputVideoCodec = "copy"
	job.VideoStream.Codec = "hevc"
	job.VideoStream.NalLengthSize = "0"
	job.VideoQuality.ForcedKeyFrameSeconds = 3

	args, err := BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3}, progressiveTestCapabilities())
	if err != nil {
		t.Fatalf("BuildHLSArgs fMP4 HEVC copy: %v", err)
	}
	if !hasArgumentValue(args, "-codec:v:0", "copy") || !hasArgumentValue(args, "-hls_segment_type", "fmp4") {
		t.Fatalf("fMP4 HEVC copy argv must retain HEVC packets in fMP4 segments: %q", args)
	}
}

func TestBuildHLSArgsAppliesDirectDolbyHEVCFMP4Tags(t *testing.T) {
	for _, test := range []struct {
		name       string
		profile    int
		transfer   string
		direct     bool
		wantTag    string
		wantStrict bool
		wantError  bool
	}{
		{name: "HLG base layer", transfer: "arib-std-b67", wantTag: "hvc1"},
		{name: "direct Dolby Vision profile five", profile: 5, direct: true, wantTag: "dvh1", wantStrict: true},
		{name: "direct Dolby Vision HLG base layer", profile: 8, transfer: "arib-std-b67", direct: true, wantTag: "hvc1", wantStrict: true},
		{name: "Dolby fallback is not copied", profile: 5, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := progressiveUnitJob()
			job.OutputPath = "/cache/hevc-master.m3u8"
			job.OutputContainer = "fmp4"
			job.OutputVideoCodec = "copy"
			job.VideoStream.Codec = "hevc"
			job.VideoStream.NalLengthSize = "0"
			job.VideoStream.DolbyVisionProfile = test.profile
			job.VideoStream.ColorTransfer = test.transfer
			job.VideoQuality.ForcedKeyFrameSeconds = 3

			args, err := BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3, DirectDolbyVision: test.direct}, progressiveTestCapabilities())
			if (err != nil) != test.wantError {
				t.Fatalf("BuildHLSArgs error = %v, want error = %t", err, test.wantError)
			}
			if test.wantError {
				return
			}
			if !hasArgumentValue(args, "-tag:v:0", test.wantTag) {
				t.Fatalf("HEVC copy tag argv = %q, want %s", args, test.wantTag)
			}
			gotStrict := hasArgumentValue(args, "-strict", "-2")
			if gotStrict != test.wantStrict {
				t.Fatalf("HEVC copy strict flag = %t, want %t; argv = %q", gotStrict, test.wantStrict, args)
			}
		})
	}
}

// A live request may carry no bitrate and the source may have none either.
// The encoder then runs on CRF alone; -maxrate 0 is invalid.
func TestTranscodesWithoutBitrateOmitRateControl(t *testing.T) {
	for _, builder := range []struct {
		name  string
		build func(EncodingJob) ([]string, error)
	}{
		{name: "progressive", build: func(job EncodingJob) ([]string, error) {
			return BuildProgressiveArgs(job, progressiveTestCapabilities())
		}},
		{name: "HLS", build: func(job EncodingJob) ([]string, error) {
			return BuildHLSArgs(HLSJob{EncodingJob: job, SegmentLengthSeconds: 3}, progressiveTestCapabilities())
		}},
	} {
		t.Run(builder.name, func(t *testing.T) {
			job := progressiveUnitJob()
			if builder.name == "HLS" {
				job.OutputPath = "/cache/master.m3u8"
				job.VideoQuality.ForcedKeyFrameSeconds = 3
			}
			job.OutputVideoBitrate = 0
			args, err := builder.build(job)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if argumentValue(t, args, "-crf") != "23" {
				t.Errorf("argv = %q, want -crf 23", args)
			}
			for _, option := range args {
				if option == "-maxrate" || option == "-bufsize" {
					t.Errorf("argv = %q, want no %s without a bitrate", args, option)
				}
			}
		})
	}
}
