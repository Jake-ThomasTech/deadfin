// SPDX-License-Identifier: GPL-2.0-only

package transcodeworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/deadfin-org/deadfin/internal/encoding"
	"github.com/deadfin-org/deadfin/internal/ffmpeg"
)

// The hardware tier tests (#854) build real argv with internal/encoding
// for the issue's fleet: worker A is an Intel Arc that decodes and encodes
// AV1, worker B an Intel iGPU without AV1.

// arcEncoding is a jellyfin-ffmpeg on an Intel Arc.
func arcEncoding() encoding.Capabilities {
	caps := intelEncoding()
	caps.Encoders = []string{"av1_qsv", "av1_vaapi", "h264_qsv", "h264_vaapi", "hevc_qsv", "libx264"}
	caps.Decoders = []string{"av1_qsv", "h264_qsv", "hevc_qsv", "vp9_qsv"}
	return caps
}

func arcWorker(pools ...string) Capabilities {
	return workerCaps("a", arcEncoding(), &encoding.HostHardware{Type: "qsv", VaapiDevice: renderNode, QsvDevice: renderNode, DecodingCodecs: []string{"h264", "hevc", "av1"}}, pools...)
}

func igpuWorker(pools ...string) Capabilities {
	return workerCaps("b", intelEncoding(), &encoding.HostHardware{Type: "qsv", VaapiDevice: renderNode, QsvDevice: renderNode, DecodingCodecs: []string{"h264", "hevc"}}, pools...)
}

// av1Job is a 1080p AV1 source transcoded to target.
func (f family) av1Job(target string) encoding.HLSJob {
	job := f.hlsJob(false)
	job.VideoStream.Codec, job.VideoStream.Profile = "av1", "Main"
	job.OutputVideoCodec = target
	if target != "h264" {
		job.OutputVideoProfile, job.OutputVideoLevel = "main", 0
	}
	return job
}

func TestArgvHardwareTier(t *testing.T) {
	f := newFamily(t, "media")
	job := f.hardwareJob(t, f.av1Job("h264"))
	plan := newJobPlan(job)
	for _, tc := range []struct {
		name string
		caps Capabilities
		want int
	}{
		{"arc decodes and encodes AV1 in hardware", arcWorker(), TierFull},
		{"igpu decodes AV1 in software", igpuWorker(), TierEncode},
		{"software worker", workerCaps("cpu", softwareEncoding(true), nil), TierSoftware},
	} {
		form := plan.forWorker(tc.caps, profileKey(tc.caps))
		if form.err != nil {
			t.Fatalf("%s: %v", tc.name, form.err)
		}
		if form.tier != tc.want {
			t.Errorf("%s: tier %d, want %d; argv %s", tc.name, form.tier, tc.want, strings.Join(form.spec.Args, " "))
		}
	}
	if plan.server.tier != TierSoftware {
		t.Errorf("server tier = %d, want software", plan.server.tier)
	}
	for name, tc := range map[string]struct {
		need requirement
		want int
	}{
		"hwaccel and hardware encoder":     {requirement{hwaccels: []string{"cuda"}, hwdecode: true, encoders: []string{"h264_nvenc"}}, TierFull},
		"init_hw_device only":              {requirement{hwaccels: []string{"qsv"}, encoders: []string{"h264_qsv"}}, TierEncode},
		"hardware decode, software encode": {requirement{hwaccels: []string{"vaapi"}, hwdecode: true, encoders: []string{"libx264"}}, TierEncode},
		"software":                         {requirement{encoders: []string{"libx264"}}, TierSoftware},
	} {
		if got := tc.need.tier(); got != tc.want {
			t.Errorf("%s: tier %d, want %d", name, got, tc.want)
		}
	}
}

func TestArgvRequirementSeesAHardwareDecoder(t *testing.T) {
	f := newFamily(t, "media")
	request := f.job()
	request.Args = []string{"-c:v", "hevc_cuvid", "-i", "file:" + f.path, "-c:v", "h264_nvenc", "-hls_segment_filename", request.SegmentPattern, "-y", request.Playlist}
	_, need, err := buildJobSpec(request)
	if err != nil {
		t.Fatal(err)
	}
	if !need.hwdecode || need.tier() != TierFull {
		t.Fatalf("need = %+v, tier %d", need, need.tier())
	}
}

// TestAV1SourceGoesToTheWorkerThatDecodesAV1: acceptance 1 of #854. B is
// older and idle, so pack and spread alone would take it; A runs the whole
// job in hardware and wins every time.
func TestAV1SourceGoesToTheWorkerThatDecodesAV1(t *testing.T) {
	for _, strategy := range []Strategy{StrategyPack, StrategySpread} {
		t.Run(string(strategy), func(t *testing.T) {
			token := mustToken(t)
			hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst, Strategy: strategy}}, token)
			b := igpuWorker()
			b.MaxJobs = 4
			startTestAgent(t, hub, token, "b", b)
			hub.waitEvent(t, EventRegistered)
			hub.clock.Advance(time.Second)
			a := arcWorker()
			a.MaxJobs = 4
			arc := startTestAgent(t, hub, token, "a", a)
			hub.waitEvent(t, EventRegistered)
			var processes []ffmpeg.RemoteProcess
			for range 3 {
				f := newFamily(t, "media")
				processes = append(processes, startJob(t, hub, f.hardwareJob(t, f.av1Job("h264"))))
				if event := hub.waitEvent(t, EventJobPlaced); event.Name != "a" {
					t.Fatalf("placed on %q, want a", event.Name)
				}
				if args := launchedArgs(t, arc); !strings.Contains(args, "h264_qsv") {
					t.Fatalf("a argv = %s", args)
				}
			}
			for _, process := range processes {
				process.Kill()
				_ = waitResult(t, process)
			}
		})
	}
}

// TestTierFallsBackToTheLowerTierWorker: acceptance 2 of #854. With A
// busy, and then with A draining, the job goes to B at tier 1; it does not
// queue or fail.
func TestTierFallsBackToTheLowerTierWorker(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersOnly}}, token)
	arc := startTestAgent(t, hub, token, "a", arcWorker())
	hub.waitEvent(t, EventRegistered)
	b := igpuWorker()
	b.MaxJobs = 2
	igpu := startTestAgent(t, hub, token, "b", b)
	hub.waitEvent(t, EventRegistered)

	f := newFamily(t, "media0")
	first := startJob(t, hub, f.hardwareJob(t, f.av1Job("h264")))
	if event := hub.waitEvent(t, EventJobPlaced); event.Name != "a" {
		t.Fatalf("first job on %q, want a", event.Name)
	}
	_ = arc.process(t)
	f = newFamily(t, "media1")
	second := startJob(t, hub, f.hardwareJob(t, f.av1Job("h264")))
	if event := hub.waitEvent(t, EventJobPlaced); event.Name != "b" {
		t.Fatalf("job with a busy on %q, want b", event.Name)
	}
	_ = igpu.process(t)

	first.Kill()
	_ = waitResult(t, first)
	arc.agent.Drain()
	hub.waitEvent(t, EventDraining)
	f = newFamily(t, "media2")
	third := startJob(t, hub, f.hardwareJob(t, f.av1Job("h264")))
	if event := hub.waitEvent(t, EventJobPlaced); event.Name != "b" {
		t.Fatalf("job with a draining on %q, want b", event.Name)
	}
	for _, process := range []ffmpeg.RemoteProcess{second, third} {
		process.Kill()
		_ = waitResult(t, process)
	}
}

// remoteOnlyJob is the remote-only form of an AV1 output that only enc
// encodes (#854): the server argv is built for that worker.
func (f family) remoteOnlyJob(t *testing.T, job encoding.HLSJob, enc ffmpeg.RemoteEncoder) ffmpeg.RemoteHLSJob {
	t.Helper()
	server := job
	server.Hardware = job.Hardware.ForHost(enc.Host)
	args, err := encoding.BuildHLSArgs(server, enc.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	remote := f.hardwareJob(t, f.hlsJob(false))
	remote.Args = args
	remote.Facts = ffmpeg.JobFacts(encoding.PlacementFacts(job, softwareEncoding(true)))
	remote.Build = func(caps encoding.Capabilities, host encoding.HostHardware) ([]string, error) {
		worker := job
		worker.Hardware = job.Hardware.ForHost(host)
		return encoding.BuildHLSArgs(worker, caps)
	}
	remote.RemoteOnly = true
	return remote
}

// TestFleetEncodesACodecThatTheServerCannot: acceptance 3 and 4 of #854.
// The server has no AV1 encoder; A has av1_qsv. A remote-only AV1 job goes
// to A. With A gone, the fleet offers no AV1 encoder, and a pinned
// remote-only job waits for the queue timeout and then fails as usual.
func TestFleetEncodesACodecThatTheServerCannot(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst}}, token)
	startTestAgent(t, hub, token, "b", igpuWorker())
	hub.waitEvent(t, EventRegistered)
	arc := startTestAgent(t, hub, token, "a", arcWorker())
	hub.waitEvent(t, EventRegistered)

	f := newFamily(t, "media")
	hevc := f.hlsJob(true)
	hevc.OutputVideoCodec, hevc.OutputVideoProfile, hevc.OutputVideoLevel = "av1", "main", 0
	options := serverHardware()
	options.AllowAv1Encoding = true
	if encoding.CanEncodeVideo("av1", softwareEncoding(true), options) {
		t.Fatal("the server encodes AV1")
	}
	facts := ffmpeg.JobFacts(encoding.PlacementFacts(hevc, softwareEncoding(true)))
	if !hub.hub.RemotePlacement() {
		t.Fatal("workers-first is not a remote placement")
	}
	enc, ok := hub.hub.RemoteVideoEncoder(facts, f.path, options)
	if !ok || enc.Capabilities.H264Encoder == "" {
		t.Fatalf("RemoteVideoEncoder = %+v, %v; want worker a", enc, ok)
	}
	job := f.remoteOnlyJob(t, hevc, enc)
	process := startJob(t, hub, job)
	if event := hub.waitEvent(t, EventJobPlaced); event.Name != "a" {
		t.Fatalf("placed on %q, want a", event.Name)
	}
	if args := launchedArgs(t, arc); !strings.Contains(args, "av1_qsv") {
		t.Fatalf("a argv = %s", args)
	}
	process.Kill()
	_ = waitResult(t, process)

	arc.kill()
	hub.waitEvent(t, EventDisconnected)
	if enc, ok := hub.hub.RemoteVideoEncoder(facts, f.path, options); ok {
		t.Fatalf("RemoteVideoEncoder with a gone = %+v", enc)
	}
	f = newFamily(t, "media1")
	result := startAsync(context.Background(), hub, f.remoteOnlyJob(t, hevc, enc))
	hub.waitEvent(t, EventJobQueued)
	stillWaiting(t, result)
	hub.clock.Advance(DefaultQueueTimeout)
	if got := receiveStart(t, result); !errors.Is(got.err, ffmpeg.ErrRemoteRefused) {
		t.Fatalf("remote-only job without a worker: %v, want ErrRemoteRefused", got.err)
	}
}

// TestRemoteOnlyJobSkipsAWorkerWithoutEncodingCapabilities: a worker that
// runs the server argv as it is cannot run a remote-only job.
func TestRemoteOnlyJobSkipsAWorkerWithoutEncodingCapabilities(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst}}, token)
	startTestAgent(t, hub, token, "plain", Capabilities{MaxJobs: 1})
	hub.waitEvent(t, EventRegistered)
	f := newFamily(t, "media")
	hevc := f.hlsJob(true)
	hevc.OutputVideoCodec, hevc.OutputVideoProfile, hevc.OutputVideoLevel = "av1", "main", 0
	arc := arcWorker()
	job := f.remoteOnlyJob(t, hevc, ffmpeg.RemoteEncoder{WorkerID: "gone", Capabilities: *arc.Encoding, Host: *arc.Hardware})
	result := startAsync(context.Background(), hub, job)
	hub.waitEvent(t, EventJobQueued)
	hub.clock.Advance(DefaultQueueTimeout)
	if got := receiveStart(t, result); !errors.Is(got.err, ffmpeg.ErrRemoteRefused) {
		t.Fatalf("remote-only job: %v, want ErrRemoteRefused", got.err)
	}
}

// TestLocalModeOffersNoRemoteEncoder: acceptance 5 of #854. In local mode
// the codec choice uses the server's capabilities only.
func TestLocalModeOffersNoRemoteEncoder(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeLocal}}, token)
	startTestAgent(t, hub, token, "a", arcWorker())
	hub.waitEvent(t, EventRegistered)
	options := serverHardware()
	options.AllowAv1Encoding = true
	if hub.hub.RemotePlacement() {
		t.Fatal("local mode is a remote placement")
	}
	if enc, ok := hub.hub.RemoteVideoEncoder(ffmpeg.JobFacts{SourceCodec: "hevc", TargetCodec: "av1"}, "", options); ok {
		t.Fatalf("RemoteVideoEncoder in local mode = %+v", enc)
	}
}

// TestRemoteVideoEncoderHonorsRulePools: the encoder comes from the pools
// that a matching rule names.
func TestRemoteVideoEncoderHonorsRulePools(t *testing.T) {
	token := mustToken(t)
	hub := newTestHubWithConfig(t, HubConfig{Policy: Policy{Mode: ModeWorkersFirst,
		Rules: []Rule{{Name: "hdr", Match: Match{HDR: []string{"hdr10"}}, Pools: []string{"cpu"}}, {Name: "keep", Match: Match{SourceCodec: []string{"vp9"}}, Local: true}}}}, token)
	startTestAgent(t, hub, token, "a", arcWorker("gpu"))
	hub.waitEvent(t, EventRegistered)
	options := serverHardware()
	options.AllowAv1Encoding = true
	if _, ok := hub.hub.RemoteVideoEncoder(ffmpeg.JobFacts{SourceCodec: "hevc", HDR: "hdr10", TargetCodec: "av1"}, "", options); ok {
		t.Fatal("encoder outside the rule's pools")
	}
	if _, ok := hub.hub.RemoteVideoEncoder(ffmpeg.JobFacts{SourceCodec: "hevc", HDR: "sdr", TargetCodec: "av1"}, "", options); !ok {
		t.Fatal("no encoder in the default pools")
	}
	if _, ok := hub.hub.RemoteVideoEncoder(ffmpeg.JobFacts{SourceCodec: "vp9", HDR: "sdr", TargetCodec: "av1"}, "", options); !ok {
		t.Fatal("a local rule hid the fleet encoder")
	}
}
