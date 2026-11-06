// SPDX-License-Identifier: GPL-2.0-only

package playback

import (
	"strings"
	"testing"

	"github.com/deadfin-org/deadfin/internal/encoding"
)

// TestHLSWorkerArgsBuildsForTheWorkerHost covers #622: a video transcode
// gets an argv builder that uses the worker's capabilities and hardware;
// a remux and an audio-only job keep the server argv.
func TestHLSWorkerArgsBuildsForTheWorkerHost(t *testing.T) {
	job := encoding.HLSJob{
		EncodingJob: encoding.EncodingJob{
			InputPath: "/media/a.mkv", InputContainer: "mkv", OutputPath: "/cache/transcodes/abc.m3u8", OutputContainer: "ts",
			VideoStream: encoding.VideoStream{Index: 0, Codec: "h264", BitDepth: 8, NalLengthSize: "4", Width: 1920, Height: 1080,
				PixelFormat: "yuv420p", Profile: "High", RangeType: "SDR", RealFrameRate: 24},
			AudioStream:      encoding.AudioStream{Index: 1, Codec: "aac", Channels: 2},
			OutputVideoCodec: "h264", OutputAudioCodec: "copy", OutputVideoProfile: "high", OutputVideoLevel: 41,
			OutputWidth: 1280, OutputHeight: 720, OutputVideoBitrate: 4_000_000, CopyTimestamps: true,
			VideoQuality: encoding.VideoQuality{Preset: "veryfast", CRF: 23, ForcedKeyFrameSeconds: 3, X264Options: "subme=0"},
			Hardware: encoding.HardwareOptions{Type: encoding.HardwareAccelerationNone, EnableHardwareEncoding: true,
				HardwareDecodingCodecs: []string{"h264"}, EncoderPreset: "auto"},
		},
		SegmentLengthSeconds: 3,
	}
	build := hlsWorkerArgs(job)
	if build == nil {
		t.Fatal("a video transcode has no worker argv builder")
	}
	caps := encoding.Capabilities{H264Encoder: "libx264", EncoderMajorVersion: 8, EncoderMinorVersion: 1, AACEncoders: []string{"aac"},
		Encoders: []string{"h264_qsv", "libx264"}, Decoders: []string{"h264_qsv"}, HWAccels: []string{"qsv", "vaapi"}, Filters: []string{"vpp_qsv", "scale_vaapi"}}
	args, err := build(caps, encoding.HostHardware{Type: encoding.HardwareAccelerationQSV, VaapiDevice: "/dev/dri/renderD129", QsvDevice: "/dev/dri/renderD129"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "h264_qsv") || !strings.Contains(joined, "/dev/dri/renderD129") || !strings.HasSuffix(joined, "-y /cache/transcodes/abc.m3u8") {
		t.Fatalf("worker argv = %s", joined)
	}
	software, err := build(caps, encoding.HostHardware{})
	if err != nil || !strings.Contains(strings.Join(software, " "), "libx264") {
		t.Fatalf("software worker argv = %v, %v", software, err)
	}
	remux := job
	remux.OutputVideoCodec = "copy"
	if hlsWorkerArgs(remux) != nil {
		t.Fatal("a remux has a worker argv builder")
	}
	audio := job
	audio.OutputVideoCodec = ""
	if hlsWorkerArgs(audio) != nil {
		t.Fatal("an audio-only job has a worker argv builder")
	}
}
