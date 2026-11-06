// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// The hardware placement tests (#622) build real argv with
// internal/encoding: the server has no GPU, and each worker reports the
// capabilities of its own jellyfin-ffmpeg and hardware.

const renderNode = "/dev/dri/renderD128"

// softwareEncoding is a jellyfin-ffmpeg with no usable hardware family.
func softwareEncoding(tonemapx bool) encoding.Capabilities {
	caps := encoding.Capabilities{
		H264Encoder: "libx264", EncoderMajorVersion: 8, EncoderMinorVersion: 1, EncoderPatchVersion: 1,
		SupportsSubtitleExtraction: true, AACEncoders: []string{"aac"}, SupportsTonemapX: tonemapx,
		Encoders: []string{"libx264"}, Filters: []string{"subtitles"},
	}
	if tonemapx {
		caps.Filters = append(caps.Filters, "tonemapx")
	}
	return caps
}

// intelEncoding is a jellyfin-ffmpeg on an Intel iGPU.
func intelEncoding() encoding.Capabilities {
	caps := softwareEncoding(true)
	caps.VaapiDriver = "iHD"
	caps.Encoders = []string{"h264_qsv", "h264_vaapi", "hevc_qsv", "libx264"}
	caps.Decoders = []string{"h264_qsv", "hevc_qsv", "vp9_qsv"}
	caps.HWAccels = []string{"drm", "opencl", "qsv", "vaapi"}
	caps.Filters = append(caps.Filters, "deinterlace_vaapi", "hwupload_vaapi", "overlay_opencl", "overlay_qsv", "overlay_vaapi",
		"procamp_vaapi", "scale_opencl", "scale_vaapi", "tonemap_opencl", "tonemap_vaapi", "transpose_vaapi", "vpp_qsv")
	return caps
}

// nvidiaEncoding is a jellyfin-ffmpeg on an NVIDIA GPU whose NVENC encoders
// open.
func nvidiaEncoding() encoding.Capabilities {
	caps := softwareEncoding(true)
	caps.Encoders = []string{"h264_nvenc", "hevc_nvenc", "libx264"}
	caps.Decoders = []string{"h264_cuvid", "hevc_cuvid"}
	caps.HWAccels = []string{"cuda"}
	caps.Filters = append(caps.Filters, "scale_cuda", "tonemap_cuda", "yadif_cuda", "overlay_cuda", "hwupload_cuda")
	return caps
}

func workerCaps(name string, encodingCaps encoding.Capabilities, hardware *encoding.HostHardware, pools ...string) Capabilities {
	caps := Capabilities{MaxJobs: 1, Pools: pools, Encoding: &encodingCaps, Hardware: hardware, Encoders: encodingCaps.Encoders, FFmpegVersion: "8.1.1-Jellyfin"}
	if hardware != nil {
		caps.HWAccels = encodingCaps.HWAccels
	}
	_ = name
	return caps
}

// serverHardware is the server's encoding.xml: no hardware family, with
// the options an operator sets for the GPU workers.
func serverHardware() encoding.HardwareOptions {
	return encoding.HardwareOptions{
		Type:                           encoding.HardwareAccelerationNone,
		EnableHardwareEncoding:         true,
		HardwareDecodingCodecs:         []string{"h264", "hevc", "vp9", "av1"},
		EnableDecodingColorDepth10Hevc: true,
		EnableDecodingColorDepth10Vp9:  true,
		PreferSystemNativeHwDecoder:    true,
		EnableTonemapping:              true,
		Tonemapping:                    encoding.DefaultTonemapping(),
		EncoderPreset:                  "auto",
	}
}

// hlsJob is a 4K HEVC HDR10 or a 1080p H.264 SDR transcode of f's source to
// 1080p H.264.
func (f family) hlsJob(hdr bool) encoding.HLSJob {
	job := encoding.HLSJob{
		EncodingJob: encoding.EncodingJob{
			InputPath: f.path, InputContainer: "mkv", OutputPath: filepath.Join(f.dir, "abc.m3u8"), OutputContainer: "ts",
			VideoStream: encoding.VideoStream{
				Index: 0, Codec: "h264", BitDepth: 8, NalLengthSize: "4",
				Width: 1920, Height: 1080, PixelFormat: "yuv420p", Profile: "High", RangeType: "SDR", RealFrameRate: 24,
			},
			AudioStream:      encoding.AudioStream{Index: 1, Codec: "aac", Channels: 2},
			OutputVideoCodec: "h264", OutputAudioCodec: "copy", OutputVideoProfile: "high", OutputVideoLevel: 41,
			OutputWidth: 1920, OutputHeight: 1080, OutputVideoBitrate: 8_000_000, CopyTimestamps: true,
			VideoQuality: encoding.VideoQuality{Preset: "veryfast", CRF: 23, ForcedKeyFrameSeconds: 3, X264Options: "subme=0:me_range=16:rc_lookahead=10:me=hex:open_gop=0"},
			Hardware:     serverHardware(),
		},
		SegmentLengthSeconds: 3,
	}
	if hdr {
		job.VideoStream = encoding.VideoStream{
			Index: 0, Codec: "hevc", ColorTransfer: "smpte2084", BitDepth: 10, NalLengthSize: "4",
			Width: 3840, Height: 2160, PixelFormat: "yuv420p10le", Profile: "Main 10", RangeType: "HDR10", RealFrameRate: 23.976,
		}
	}
	return job
}

// hardwareJob is the remote form of job: the server argv, built with the
// server's software capabilities (jellyfin-ffmpeg with tonemapx), and the
// builder for a worker's.
func (f family) hardwareJob(t *testing.T, job encoding.HLSJob) ffmpeg.RemoteHLSJob {
	t.Helper()
	server := softwareEncoding(true)
	args, err := encoding.BuildHLSArgs(job, server)
	if err != nil {
		t.Fatal(err)
	}
	remote := f.job()
	remote.Args = args
	remote.Facts = ffmpeg.JobFacts(encoding.PlacementFacts(job, server))
	remote.Build = func(caps encoding.Capabilities, host encoding.HostHardware) ([]string, error) {
		worker := job
		worker.Hardware = job.Hardware.ForHost(host)
		return encoding.BuildHLSArgs(worker, caps)
	}
	return remote
}

func launchedArgs(t *testing.T, worker *testAgent) string {
	t.Helper()
	return strings.Join(worker.process(t).options.Args, " ")
}

// TestPlacementBuildsAQSVArgvOnAServerWithoutAGPU: the server's own argv
// is libx264; the job placed on a QSV worker runs h264_qsv on the worker's
// render node.
func TestPlacementBuildsAQSVArgvOnAServerWithoutAGPU(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{}, token)
	qsv := startTestAgent(t, hub, token, "qsv", workerCaps("qsv", intelEncoding(), &encoding.HostHardware{Type: "qsv", VaapiDevice: renderNode, QsvDevice: renderNode}))
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	job := f.hardwareJob(t, f.hlsJob(false))
	if server := strings.Join(job.Args, " "); !strings.Contains(server, "libx264") || strings.Contains(server, "qsv") {
		t.Fatalf("server argv = %s, want libx264", server)
	}
	process := startJob(t, hub, job)
	args := launchedArgs(t, qsv)
	for _, want := range []string{"-init_hw_device vaapi=va:" + renderNode, "-init_hw_device qsv=qs@va", "-codec:v:0 h264_qsv"} {
		if !strings.Contains(args, want) {
			t.Fatalf("worker argv lacks %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "libx264") || strings.Contains(args, f.path) {
		t.Fatalf("worker argv = %s", args)
	}
	process.Kill()
	_ = waitResult(t, process)
}

// TestToneMappingRulePlacesOnAWorkerWithTonemapx: a rule sends tone
// mapping to the gpu pool. The older worker there has no tonemapx, so no
// argv builds for it; the worker that reports tonemapx gets the job with
// its tone-mapping filter chain.
func TestToneMappingRulePlacesOnAWorkerWithTonemapx(t *testing.T) {
	token := mustToken(t)
	yes := true
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"cpu"},
		Rules: []Rule{{Name: "hdr", Match: Match{NeedsToneMap: &yes}, Pools: []string{"gpu"}}}}}, token)
	startTestAgent(t, hub, token, "cpu", workerCaps("cpu", softwareEncoding(true), nil, "cpu"))
	hub.waitEvent(t, EventRegistered)
	old := startTestAgent(t, hub, token, "old", workerCaps("old", softwareEncoding(false), nil, "gpu"))
	hub.waitEvent(t, EventRegistered)
	hub.clock.Advance(time.Second)
	gpu := startTestAgent(t, hub, token, "gpu", workerCaps("gpu", softwareEncoding(true), nil, "gpu"))
	hub.waitEvent(t, EventRegistered)

	f := newFamily(t, "media")
	job := f.hardwareJob(t, f.hlsJob(true))
	if !job.Facts.NeedsToneMap {
		t.Fatalf("facts = %+v", job.Facts)
	}
	process := startJob(t, hub, job)
	if event := hub.waitEvent(t, EventJobPlaced); event.Name != "gpu" || event.Detail != "hdr" {
		t.Fatalf("placed on %q by rule %q, want gpu by hdr", event.Name, event.Detail)
	}
	if args := launchedArgs(t, gpu); !strings.Contains(args, "tonemapx=tonemap=bt2390") {
		t.Fatalf("gpu worker argv lacks tonemapx:\n%s", args)
	}
	select {
	case <-old.launched:
		t.Fatal("the worker without tonemapx launched the tone-mapping job")
	default:
	}
	process.Kill()
	_ = waitResult(t, process)
}

// TestFullNVENCWorkerFallsBackToQSVThenLocal: the NVENC worker has one
// hardware session. The first job takes it; the second goes to the QSV
// worker in the same pool; the third runs locally in software.
func TestFullNVENCWorkerFallsBackToQSVThenLocal(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst, DefaultPools: []string{"gpu"}}}, token)
	nvencCaps := workerCaps("nvenc", nvidiaEncoding(), &encoding.HostHardware{Type: "nvenc"}, "gpu")
	nvencCaps.MaxJobs, nvencCaps.HardwareSessions = 4, 1
	nvenc := startTestAgent(t, hub, token, "nvenc", nvencCaps)
	hub.waitEvent(t, EventRegistered)
	hub.clock.Advance(time.Second)
	qsv := startTestAgent(t, hub, token, "qsv", workerCaps("qsv", intelEncoding(), &encoding.HostHardware{Type: "qsv", VaapiDevice: renderNode, QsvDevice: renderNode}, "gpu"))
	hub.waitEvent(t, EventRegistered)

	var processes []ffmpeg.RemoteProcess
	for index, want := range []struct {
		worker  *testAgent
		encoder string
	}{{nvenc, "h264_nvenc"}, {qsv, "h264_qsv"}} {
		f := newFamily(t, fmt.Sprint("media", index))
		processes = append(processes, startJob(t, hub, f.hardwareJob(t, f.hlsJob(false))))
		if args := launchedArgs(t, want.worker); !strings.Contains(args, want.encoder) {
			t.Fatalf("job %d argv lacks %s:\n%s", index, want.encoder, args)
		}
	}
	f := newFamily(t, "media2")
	_, err := hub.hub.StartHLS(t.Context(), f.hardwareJob(t, f.hlsJob(false)))
	if !errors.Is(err, ffmpeg.ErrNoRemoteRunner) {
		t.Fatalf("third job: %v, want ErrNoRemoteRunner (local software)", err)
	}
	if event := hub.waitEvent(t, EventFallback); event.Detail != "local_cpu:no-worker" {
		t.Fatalf("fallback = %q", event.Detail)
	}
	for _, process := range processes {
		process.Kill()
		_ = waitResult(t, process)
	}
}

// TestHardwareWorkerIsPreferredOverSoftware: in one pool, a worker whose
// argv uses its hardware wins over an older software worker.
func TestHardwareWorkerIsPreferredOverSoftware(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{}, token)
	startTestAgent(t, hub, token, "cpu", workerCaps("cpu", softwareEncoding(true), nil))
	hub.waitEvent(t, EventRegistered)
	hub.clock.Advance(time.Second)
	qsv := startTestAgent(t, hub, token, "qsv", workerCaps("qsv", intelEncoding(), &encoding.HostHardware{Type: "qsv", VaapiDevice: renderNode, QsvDevice: renderNode}))
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	process := startJob(t, hub, f.hardwareJob(t, f.hlsJob(false)))
	if args := launchedArgs(t, qsv); !strings.Contains(args, "h264_qsv") {
		t.Fatalf("qsv argv = %s", args)
	}
	process.Kill()
	_ = waitResult(t, process)
}

func TestWorkerHardwareValidation(t *testing.T) {
	for name, caps := range map[string]Capabilities{
		"type":     {MaxJobs: 1, Hardware: &encoding.HostHardware{Type: "amf"}},
		"device":   {MaxJobs: 1, Hardware: &encoding.HostHardware{Type: "qsv", QsvDevice: "/tmp/x"}},
		"operand":  {MaxJobs: 1, Hardware: &encoding.HostHardware{Type: "vaapi", VaapiDevice: "/dev/dri/renderD128,driver=x"}},
		"sessions": {MaxJobs: 1, HardwareSessions: -1},
	} {
		if validateHardware(caps) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateHardware(Capabilities{MaxJobs: 1, Hardware: &encoding.HostHardware{Type: "qsv", QsvDevice: renderNode}}); err != nil {
		t.Fatal(err)
	}
}
